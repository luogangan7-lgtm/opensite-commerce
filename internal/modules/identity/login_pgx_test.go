//go:build pgx

// Actual HTTP/PG integration uses disposable databases and dedicated auth logins.
// Dependencies: psql admin fixtures and scoped identity transactions, real migrations, identity public HTTP port.
// Invariants: counters, user/session admission and conditional rotation are proven
// by production SQL; concurrency waits on witnessed PG locks, never sleep order.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	identityadapter "github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

// testLoginDB exposes test SQL only inside a synchronous maintenance callback.
// A Row keeps query inputs, never a transaction; Scan completes before commit.
type testLoginDB struct {
	runner *identityadapter.IdentityRunner
}
type testLoginRow struct{ scan func(...any) error }

func (r testLoginRow) Scan(dest ...any) error { return r.scan(dest...) }
func (d *testLoginDB) Exec(ctx context.Context, sql string, args ...any) (identityadapter.CommandTag, error) {
	var tag identityadapter.CommandTag
	err := d.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		tag, err = tx.Exec(ctx, sql, args...)
		return err
	})
	return tag, err
}
func (d *testLoginDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return testLoginRow{scan: func(dest ...any) error {
		return d.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, sql, args...).Scan(dest...)
		})
	}}
}

// Credentials stay in child environments or SQL stdin; subprocess diagnostics
// are discarded because either a driver or installer may repeat its inputs.
func fixturePSQL(ctx context.Context, dsn, sql string, args ...string) error {
	command := exec.CommandContext(ctx, "psql", append([]string{"-X", "-q", "-v", "ON_ERROR_STOP=1"}, args...)...)
	environment, err := fixturePSQLEnvironment(dsn)
	if err != nil {
		return err
	}
	command.Env = environment
	command.Stdin = strings.NewReader(sql)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command.Run()
}

// Native libpq reads a database name from PGDATABASE, not a connection URI.
// Strip all inherited PG settings, then derive fields from the fixture DSN;
// DATABASE_URL remains available to the local test shim, never passed in argv.
func fixturePSQLEnvironment(dsn string) ([]string, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || u.User == nil || u.Path == "" || u.Path == "/" {
		return nil, errors.New("fixture DSN invalid")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("fixture DSN invalid")
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	secret, _ := u.User.Password()
	fields := map[string]string{
		"DATABASE_URL": dsn,
		"PGHOST":       u.Hostname(),
		"PGPORT":       port,
		"PGUSER":       u.User.Username(),
		"PGPASSWORD":   secret,
		"PGDATABASE":   strings.TrimPrefix(u.Path, "/"),
	}
	for parameter, key := range map[string]string{
		"sslmode": "PGSSLMODE", "connect_timeout": "PGCONNECT_TIMEOUT",
		"sslrootcert": "PGSSLROOTCERT", "sslcert": "PGSSLCERT", "sslkey": "PGSSLKEY",
		"application_name": "PGAPPNAME", "options": "PGOPTIONS",
		"target_session_attrs": "PGTARGETSESSIONATTRS",
	} {
		if value := query.Get(parameter); value != "" {
			fields[key] = value
		}
	}
	environment := make([]string, 0, len(os.Environ())+len(fields))
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(key, "PG") && key != "DATABASE_URL" {
			environment = append(environment, value)
		}
	}
	for key, value := range fields {
		environment = append(environment, key+"="+value)
	}
	return environment, nil
}

func setupLoginPG(t *testing.T) (context.Context, *testLoginDB, string, LoginConfig) {
	t.Helper()
	source := os.Getenv("DATABASE_URL")
	if source == "" {
		pgxtest.Skip(t, "NOT_RUN: actual expendable PostgreSQL DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	fixtureURL, e := url.Parse(source)
	if e != nil {
		t.Fatal("fixture DSN invalid")
	}
	var entropy [16]byte
	if _, e = rand.Read(entropy[:]); e != nil {
		t.Fatal("fixture randomness unavailable")
	}
	suffix := hex.EncodeToString(entropy[:])
	database := "osc_c02b_" + suffix
	role := database + "_auth"
	roleSecret := base64.RawURLEncoding.EncodeToString(entropy[:])
	if fixturePSQL(ctx, source, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()+";") != nil {
		t.Fatal("fixture database creation failed")
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if fixturePSQL(cleanupCtx, source, "DROP DATABASE IF EXISTS "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE);") != nil {
			t.Error("fixture database cleanup failed")
		}
		if fixturePSQL(cleanupCtx, source, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize()+";") != nil {
			t.Error("fixture role cleanup failed")
		}
	})
	fixtureURL.Path = "/" + database
	fixtureDSN := fixtureURL.String()
	migrations, e := filepath.Glob("../../../migrations/[0-9][0-9][0-9]_*.sql")
	if e != nil || len(migrations) == 0 {
		t.Fatal("no fixture migrations")
	}
	sort.Strings(migrations)
	maintenance, maintenanceErr := identityadapter.OpenIdentity(ctx, source)
	if maintenanceErr != nil {
		t.Fatal("fixture maintenance connection unavailable")
	}
	defer maintenance.Close(context.Background())
	if initializationErr := pgxtest.WithFixtureInitialization(ctx, maintenance.InMaintenance, func(ctx context.Context) error {
		for _, path := range migrations {
			path, e = filepath.Abs(path)
			if e != nil {
				return errors.New("fixture migration path invalid")
			}
			if strings.HasSuffix(path, "_queue_authorization.sql") {
				installer := exec.CommandContext(ctx, "bash", "../../../scripts/migrate_river.sh")
				installer.Env = append(os.Environ(), "DATABASE_URL="+fixtureDSN)
				installer.Stdout, installer.Stderr = io.Discard, io.Discard
				if installer.Run() != nil {
					return errors.New("fixture River installation failed")
				}
			}
			if fixturePSQL(ctx, fixtureDSN, "", "-1", "-f", path) != nil {
				return fmt.Errorf("migration failed: %s", filepath.Base(path))
			}
		}
		// Generated random password is a local fixture; never a command argument.
		if fixturePSQL(ctx, source, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN INHERIT PASSWORD '"+roleSecret+"'; GRANT app_auth TO "+pgx.Identifier{role}.Sanitize()+";") != nil {
			return errors.New("fixture login unavailable")
		}
		return nil
	}); initializationErr != nil {
		t.Fatal("fixture initialization failed", initializationErr)
	}
	runner, e := identityadapter.OpenIdentity(ctx, fixtureDSN)
	if e != nil {
		t.Fatal("fixture database unavailable")
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer closeCancel()
		_ = runner.Close(closeCtx)
	})
	var key [32]byte
	if _, e = rand.Read(key[:]); e != nil {
		t.Fatal("fixture key unavailable")
	}
	authURL := *fixtureURL
	authURL.User = url.UserPassword(role, roleSecret)
	return ctx, &testLoginDB{runner: runner}, authURL.String(), LoginConfig{Origin: "https://admin.example.test", SigningKey: key[:]}
}
func safeCode(e error) string {
	if v, ok := e.(interface{ SQLState() string }); ok {
		return v.SQLState()
	}
	return "driver error"
}
func fixtureRouter(t *testing.T, ctx context.Context, dsn string, cfg LoginConfig) (*LoginModule, *platform.Router) {
	t.Helper()
	m, e := OpenLogin(ctx, dsn, cfg)
	if e != nil {
		diagnostic, connectErr := identityadapter.OpenIdentity(ctx, dsn)
		if connectErr == nil {
			var admitted any
			admissionErr := diagnostic.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
				value, err := db.New(tx).ValidateAuthConnection(ctx)
				admitted = value
				return err
			})
			t.Log("auth admission", admitted, safeCode(admissionErr))
			_ = diagnostic.Close(ctx)
		}
		t.Fatal("dedicated auth login refused")
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	r := platform.New(m.Authenticator(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, route := range m.Routes() {
		if e = r.Register(route.Spec, route.Handler); e != nil {
			t.Fatal(e)
		}
	}
	return m, r
}
func requestPG(r *platform.Router, method, path, body, csrf, ip string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	q := httptest.NewRequest(method, path, strings.NewReader(body))
	q.RemoteAddr = net.JoinHostPort(ip, "12345")
	q.Header.Set("Origin", "https://admin.example.test")
	q.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		q.Header.Set("X-CSRF-Token", csrf)
	}
	for _, c := range cookies {
		q.AddCookie(c)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, q)
	return w
}
func cookiePG(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatal("expected cookie absent", name)
	return nil
}
func challengePG(t *testing.T, r *platform.Router) (*http.Cookie, string) {
	t.Helper()
	w := requestPG(r, "GET", "/api/v1/auth/login", "", "", challengeLoginIP)
	if w.Code != 200 {
		t.Fatal("challenge refused", w.Code)
	}
	var dto struct {
		Token string `json:"csrf_token"`
	}
	if json.Unmarshal(w.Body.Bytes(), &dto) != nil || dto.Token == "" {
		t.Fatal("challenge missing")
	}
	return cookiePG(t, w, "__Host-osc_login_csrf"), dto.Token
}
func seedLoginPG(t *testing.T, ctx context.Context, db *testLoginDB, email string) string {
	t.Helper()
	phc := seededLoginPHC()
	var id string
	if e := db.QueryRow(ctx, "INSERT INTO app.users(email,password_hash) VALUES($1,$2) RETURNING id::text", email, phc).Scan(&id); e != nil {
		t.Fatal("fixture user failed")
	}
	return id
}
func TestLoginHTTPPostgres(t *testing.T) {
	ctx, db, dsn, cfg := setupLoginPG(t)
	_, router := fixtureRouter(t, ctx, dsn, cfg)
	user := seedLoginPG(t, ctx, db, "owner@example.test")
	t.Run("unknown_and_wrong_password_same_failure", func(t *testing.T) {
		for i, email := range []string{"owner@example.test", "unknown@example.test"} {
			w := loginWithInvalidSeededCredential(t, router, email, failedLoginIPs[i])
			if w.Code != 401 || strings.Contains(w.Body.String(), email) {
				t.Fatal("credential response leaked or incorrect", w.Code)
			}
			var account, pair int
			if e := db.QueryRow(ctx, "SELECT a.failure_count,l.failure_count FROM app.auth_account_attempts a JOIN app.auth_login_attempts l USING(identifier_normalized) WHERE a.identifier_normalized=$1", email).Scan(&account, &pair); e != nil || account != 1 || pair != 1 {
				t.Fatal("persistent failures differ", account, pair, safeCode(e))
			}
		}
	})
	// Independent identifiers avoid previous failure cooldown affecting success.
	user = seedLoginPG(t, ctx, db, "session@example.test")
	good := loginAsSeeded(t, router, "session@example.test", sessionLoginIP)
	if good.Code != 201 {
		t.Fatal("valid credentials refused", good.Code)
	}
	sessionCookie := cookiePG(t, good, "__Host-osc_admin_session")
	deviceCookie := cookiePG(t, good, "__Host-osc_known_device")
	var response struct {
		CSRF      string `json:"csrf_token"`
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(good.Body.Bytes(), &response) != nil {
		t.Fatal("invalid success DTO")
	}
	t.Run("cookie_and_hash_storage", func(t *testing.T) {
		if !sessionCookie.Secure || !sessionCookie.HttpOnly || sessionCookie.Path != "/" || sessionCookie.Domain != "" || sessionCookie.SameSite != http.SameSiteLaxMode {
			t.Fatal("session cookie policy")
		}
		b, e := base64.RawURLEncoding.DecodeString(sessionCookie.Value)
		if e != nil || len(b) != 32 {
			t.Fatal("opaque token bits")
		}
		want := sha256.Sum256(b)
		var actual []byte
		if e := db.QueryRow(ctx, "SELECT token_hash FROM app.auth_sessions WHERE id=$1", response.SessionID).Scan(&actual); e != nil || hex.EncodeToString(actual) != hex.EncodeToString(want[:]) {
			t.Fatal("session persisted plaintext or incorrect hash")
		}
	})
	t.Run("known_device_account_exemption", func(t *testing.T) {
		if _, e := db.Exec(ctx, "INSERT INTO app.auth_account_attempts(identifier_normalized,failure_count,cooldown_until) VALUES('session@example.test',10,clock_timestamp()+interval '60 seconds') ON CONFLICT(identifier_normalized) DO UPDATE SET cooldown_until=excluded.cooldown_until"); e != nil {
			t.Fatal("cooldown fixture failed")
		}
		w := loginAsSeeded(t, router, "session@example.test", knownSessionLoginIP, deviceCookie)
		if w.Code != 201 {
			t.Fatal("known owner locked out", w.Code)
		}
		if _, e := db.Exec(ctx, "UPDATE app.auth_account_attempts SET cooldown_until=clock_timestamp()-interval '1 second' WHERE identifier_normalized='session@example.test'"); e != nil {
			t.Fatal("fixture clear failed")
		}
	})
	t.Run("csrf_failure_then_logout_without_membership", func(t *testing.T) {
		w := requestPG(router, "POST", "/api/v1/auth/logout", "", "", sessionLoginIP, sessionCookie)
		if w.Code != 403 {
			t.Fatal("missing CSRF accepted", w.Code)
		}
		var revoked bool
		if e := db.QueryRow(ctx, "SELECT revoked_at IS NOT NULL FROM app.auth_sessions WHERE id=$1", response.SessionID).Scan(&revoked); e != nil || revoked {
			t.Fatal("CSRF denial mutated session")
		}
		w = requestPG(router, "POST", "/api/v1/auth/logout", "", response.CSRF, sessionLoginIP, sessionCookie)
		if w.Code != 204 {
			t.Fatal("membership-free logout failed", w.Code)
		}
		w = requestPG(router, "GET", "/api/v1/auth/session", "", "", sessionLoginIP, sessionCookie)
		if w.Code != 401 {
			t.Fatal("revocation not immediate", w.Code)
		}
	})
	t.Run("disabled_user_and_idle_and_absolute_denied", func(t *testing.T) {
		for i, mode := range []string{"disabled", "idle", "absolute"} {
			email := mode + "@example.test"
			uid := seedLoginPG(t, ctx, db, email)
			w := loginAsSeeded(t, router, email, expiryLoginIPs[i])
			if w.Code != 201 {
				t.Fatal("expiry fixture login refused", w.Code)
			}
			c := cookiePG(t, w, "__Host-osc_admin_session")
			var fixtureErr error
			switch mode {
			case "disabled":
				_, fixtureErr = db.Exec(ctx, "UPDATE app.users SET status='disabled' WHERE id=$1", uid)
			case "idle":
				_, fixtureErr = db.Exec(ctx, "UPDATE app.auth_sessions SET created_at=clock_timestamp()-interval '2 hours',last_seen_at=clock_timestamp()-interval '31 minutes' WHERE user_id=$1", uid)
			case "absolute":
				_, fixtureErr = db.Exec(ctx, "UPDATE app.auth_sessions SET created_at=clock_timestamp()-interval '13 hours',last_seen_at=clock_timestamp()-interval '13 hours',expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1", uid)
			}
			if fixtureErr != nil {
				t.Fatal("denial fixture mutation failed", safeCode(fixtureErr))
			}
			w = requestPG(router, "GET", "/api/v1/auth/session", "", "", repairInitialIP, c)
			if w.Code != 401 {
				t.Fatal(mode+" session admitted", w.Code)
			}
			if mode == "disabled" {
				w = loginAsSeeded(t, router, email, repairFreshIP)
				if w.Code != 401 {
					t.Fatal("disabled credential admitted", w.Code)
				}
			}
		}
	})
	_ = user
}

func TestLoginRoleAdmissionPostgres(t *testing.T) {
	ctx, db, dsn, cfg := setupLoginPG(t)
	authURL, e := url.Parse(dsn)
	if e != nil {
		t.Fatal("fixture URL unavailable")
	}
	role := pgx.Identifier{authURL.User.Username()}.Sanitize()
	for _, probe := range []struct{ name, add, remove string }{
		{"extra membership", "GRANT app_runtime TO " + role, "REVOKE app_runtime FROM " + role},
		{"create database", "ALTER ROLE " + role + " CREATEDB", "ALTER ROLE " + role + " NOCREATEDB"},
		{"extra catalog column", "GRANT SELECT(product_code) ON app.products TO " + role, "REVOKE SELECT(product_code) ON app.products FROM " + role},
	} {
		t.Run(probe.name, func(t *testing.T) {
			if _, e := db.Exec(ctx, probe.add); e != nil {
				t.Fatal("privilege probe failed", safeCode(e))
			}
			defer func() {
				if _, e := db.Exec(ctx, probe.remove); e != nil {
					t.Fatal("privilege probe restoration failed", safeCode(e))
				}
			}()
			m, e := OpenLogin(ctx, dsn, cfg)
			if e == nil || m != nil {
				if m != nil {
					_ = m.Close(ctx)
				}
				t.Fatal("overprivileged LOGIN admitted")
			}
		})
	}
	adminURL := *authURL
	adminSource, e := url.Parse(os.Getenv("DATABASE_URL"))
	if e != nil {
		t.Fatal("admin fixture URL unavailable")
	}
	adminURL.User = adminSource.User
	if m, e := OpenLogin(ctx, adminURL.String(), cfg); e == nil || m != nil {
		if m != nil {
			_ = m.Close(ctx)
		}
		t.Fatal("superuser LOGIN admitted")
	}
	_, _ = fixtureRouter(t, ctx, dsn, cfg)
}

func TestLoginDeviceRevocationPostgres(t *testing.T) {
	ctx, db, dsn, cfg := setupLoginPG(t)
	waits := 0
	cfg.Wait = func(context.Context, time.Duration) error { waits++; return nil }
	_, router := fixtureRouter(t, ctx, dsn, cfg)
	uid := seedLoginPG(t, ctx, db, "device@example.test")
	good := loginAsSeeded(t, router, "device@example.test", deviceInitialIP)
	if good.Code != 201 {
		t.Fatal("known device fixture login refused", good.Code)
	}
	device := cookiePG(t, good, "__Host-osc_known_device")
	for i, ip := range deviceFailureIPs {
		w := loginWithInvalidSeededCredential(t, router, "device@example.test", ip, device)
		if w.Code != 401 || waits != 0 {
			t.Fatal("known device incorrectly throttled or admitted", i, w.Code, waits)
		}
	}
	var failures int
	var revoked bool
	if e := db.QueryRow(ctx, "SELECT failure_count,revoked_at IS NOT NULL FROM app.known_devices WHERE user_id=$1", uid).Scan(&failures, &revoked); e != nil || failures != 5 || !revoked {
		t.Fatal("five failed device passwords not revoked", failures, revoked, safeCode(e))
	}
	// Password work can outlast the initial one-second account backoff. Make
	// the active cooldown explicit so this tests revoked-device admission,
	// independently of machine speed or concurrent race-gate CPU contention.
	if tag, e := db.Exec(ctx, "UPDATE app.auth_account_attempts SET cooldown_until=clock_timestamp()+interval '60 seconds' WHERE identifier_normalized='device@example.test'"); e != nil || tag.RowsAffected() != 1 {
		t.Fatal("revoked device cooldown fixture failed", safeCode(e))
	}
	w := loginAsSeeded(t, router, "device@example.test", deviceRecoveryIP, device)
	if w.Code != 429 || waits != 0 || w.Header().Get("Retry-After") != "60" {
		t.Fatal("revoked device retained account exemption", w.Code, waits)
	}
	if tag, e := db.Exec(ctx, "UPDATE app.auth_account_attempts SET cooldown_until=clock_timestamp()-interval '1 second' WHERE identifier_normalized='device@example.test'"); e != nil || tag.RowsAffected() != 1 {
		t.Fatal("device cooldown reset failed", safeCode(e))
	}
	w = loginAsSeeded(t, router, "device@example.test", deviceRecoveryIP, device)
	if w.Code != 201 || waits != 0 {
		t.Fatal("new login after cooldown refused", w.Code, waits)
	}
	newDevice := cookiePG(t, w, "__Host-osc_known_device")
	other := seedLoginPG(t, ctx, db, "otherdevice@example.test")
	if _, e := db.Exec(ctx, "INSERT INTO app.auth_account_attempts(identifier_normalized,failure_count,cooldown_until) VALUES('otherdevice@example.test',10,clock_timestamp()+interval '60 seconds')"); e != nil {
		t.Fatal("other account cooldown fixture failed", safeCode(e))
	}
	w = loginAsSeeded(t, router, "otherdevice@example.test", deviceOtherIP, newDevice)
	if w.Code != 429 || waits != 0 {
		t.Fatal("different-user device retained account exemption", w.Code, waits)
	}
	_ = other
}
func waitLoginLocks(t *testing.T, ctx context.Context, db *testLoginDB, query string, count int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if e := db.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE $1", query).Scan(&n); e != nil {
			t.Fatal("lock witness unavailable")
		}
		if n >= count {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("lock witness cancelled")
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("production operations never reached witnessed locks")
}

// The gate transaction never escapes its callback. The witness uses a separate
// connection while the callback retains locks until the release channel closes.
func holdLoginGate(t *testing.T, ctx context.Context, d *testLoginDB, sql string, args ...any) func() {
	t.Helper()
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- d.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, sql, args...); err != nil {
				return err
			}
			close(locked)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-locked:
	case err := <-done:
		t.Fatal("gate lock failed", safeCode(err))
	case <-ctx.Done():
		t.Fatal("gate lock cancelled")
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			close(release)
			if err := <-done; err != nil {
				t.Fatal("gate release failed", safeCode(err))
			}
		})
	}
}
func TestLoginConcurrentPostgres(t *testing.T) {
	ctx, db, dsn, cfg := setupLoginPG(t)
	_, a := fixtureRouter(t, ctx, dsn, cfg)
	_, b := fixtureRouter(t, ctx, dsn, cfg)
	uid := seedLoginPG(t, ctx, db, "race@example.test")
	t.Run("two_actual_http_failures_add_two", func(t *testing.T) {
		release := holdLoginGate(t, ctx, db, "SELECT pg_advisory_xact_lock(hashtextextended('opensite:auth:'||$1,0))", "unknownrace@example.test")
		defer release()
		ch := make(chan int, 2)
		first := prepareSeededLogin(t, a, "unknownrace@example.test", concurrentFailureIP, true)
		second := prepareSeededLogin(t, b, "unknownrace@example.test", concurrentFailureIP, true)
		go func() { ch <- first.send().Code }()
		go func() { ch <- second.send().Code }()
		witness, e := identityadapter.OpenIdentity(ctx, dsn)
		if e != nil {
			t.Fatal("witness unavailable")
		}
		defer witness.Close(context.Background())
		waitLoginLocks(t, ctx, &testLoginDB{runner: witness}, "%auth_record_failure%", 2)
		release()
		if <-ch != 401 || <-ch != 401 {
			t.Fatal("concurrent failures response")
		}
		var x, y int
		if e := db.QueryRow(ctx, "SELECT a.failure_count,l.failure_count FROM app.auth_account_attempts a JOIN app.auth_login_attempts l USING(identifier_normalized) WHERE a.identifier_normalized='unknownrace@example.test'").Scan(&x, &y); e != nil || x != 2 || y != 2 {
			t.Fatal("lost concurrent failure", x, y)
		}
	})
	t.Run("same_old_session_only_one_successor", func(t *testing.T) {
		w := loginAsSeeded(t, a, "race@example.test", rotationInitialIP)
		if w.Code != 201 {
			t.Fatal("old session fixture failed", w.Code)
		}
		old := cookiePG(t, w, "__Host-osc_admin_session")
		raw, _ := base64.RawURLEncoding.DecodeString(old.Value)
		hash := sha256.Sum256(raw)
		release := holdLoginGate(t, ctx, db, "SELECT id FROM app.auth_sessions WHERE token_hash=$1 FOR UPDATE", hash[:])
		defer release()
		first := prepareSeededLogin(t, a, "race@example.test", rotationFirstIP, false, old)
		second := prepareSeededLogin(t, b, "race@example.test", rotationSecondIP, false, old)
		ch := make(chan int, 2)
		go func() { ch <- first.send().Code }()
		go func() { ch <- second.send().Code }()
		witness, e := identityadapter.OpenIdentity(ctx, dsn)
		if e != nil {
			t.Fatal("witness unavailable")
		}
		defer witness.Close(context.Background())
		waitLoginLocks(t, ctx, &testLoginDB{runner: witness}, "%UPDATE app.auth_sessions s SET revoked_at%", 2)
		release()
		results := []int{<-ch, <-ch}
		sort.Ints(results)
		if results[0] != 201 || results[1] != 401 {
			t.Fatal("rotation response", results)
		}
		var active, total int
		if e := db.QueryRow(ctx, "SELECT count(*) FILTER(WHERE revoked_at IS NULL),count(*) FROM app.auth_sessions WHERE user_id=$1", uid).Scan(&active, &total); e != nil || active != 1 || total != 2 {
			t.Fatal("rotation created extra successors", active, total)
		}
		w = requestPG(a, "GET", "/api/v1/auth/session", "", "", rotationInitialIP, old)
		if w.Code != 401 {
			t.Fatal("old token replay admitted", w.Code)
		}
	})
}
