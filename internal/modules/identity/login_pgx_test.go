//go:build pgx

// Actual HTTP/PG integration uses disposable databases and dedicated auth logins.
// Dependencies: pgx admin fixtures, real migrations, identity public HTTP port.
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
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	identityadapter "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/identitydb/pgxadapter"
	"golang.org/x/crypto/argon2"
)

// testPassword is generated per run so no credential-shaped literal is committed.
var testPassword = func() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "t-" + hex.EncodeToString(b)
}()

func setupLoginPG(t *testing.T) (context.Context, identityadapter.Connection, string, LoginConfig) {
	t.Helper()
	source := os.Getenv("DATABASE_URL")
	if source == "" {
		t.Skip("NOT_RUN: actual expendable PostgreSQL DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	fixtureURL, e := url.Parse(source)
	if e != nil {
		t.Fatal("fixture DSN invalid")
	}
	admin, e := identityadapter.Open(ctx, source)
	if e != nil {
		t.Fatal("fixture connection failed")
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	var entropy [16]byte
	_, _ = rand.Read(entropy[:])
	suffix := hex.EncodeToString(entropy[:])
	database := "osc_c02b_" + suffix
	role := database + "_auth"
	password := base64.RawURLEncoding.EncodeToString(entropy[:])
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()); e != nil {
		t.Fatal("fixture database creation failed")
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)")
		_, _ = admin.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{role}.Sanitize())
	})
	fixtureURL.Path = "/" + database
	db, e := identityadapter.Open(ctx, fixtureURL.String())
	if e != nil {
		t.Fatal("fixture database unavailable")
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	migrations, e := filepath.Glob("../../../migrations/*.sql")
	if e != nil || len(migrations) == 0 {
		t.Fatal("no fixture migrations")
	}
	sort.Strings(migrations)
	for _, path := range migrations {
		contents, e := os.ReadFile(path)
		if e != nil {
			t.Fatal("migration unreadable")
		}
		if _, e = db.Exec(ctx, string(contents)); e != nil {
			t.Fatalf("migration failed: %s %s", filepath.Base(path), safeCode(e))
		}
	}
	// Generated random password is a local fixture; it never enters logs or CLI arguments.
	if _, e = admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN INHERIT PASSWORD '"+password+"'"); e != nil {
		t.Fatal("fixture login unavailable")
	}
	if _, e = admin.Exec(ctx, "GRANT app_auth TO "+pgx.Identifier{role}.Sanitize()); e != nil {
		t.Fatal("fixture auth membership failed")
	}
	var key [32]byte
	_, _ = rand.Read(key[:])
	authURL, parseErr := url.Parse(source)
	if parseErr != nil {
		t.Fatal("fixture URL malformed")
	}
	authURL.User = url.UserPassword(role, password)
	authURL.Path = "/" + database
	return ctx, db, authURL.String(), LoginConfig{Origin: "https://admin.example.test", SigningKey: key[:]}
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
		diagnostic, connectErr := identityadapter.Open(ctx, dsn)
		if connectErr == nil {
			admitted, admissionErr := db.New(diagnostic).ValidateAuthConnection(ctx)
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
	w := requestPG(r, "GET", "/api/v1/auth/login", "", "", "192.0.2.1")
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
func loginPG(t *testing.T, r *platform.Router, email, password, ip string, extra ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	c, csrf := challengePG(t, r)
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	return requestPG(r, "POST", "/api/v1/auth/login", string(body), csrf, ip, append(extra, c)...)
}
func seedLoginPG(t *testing.T, ctx context.Context, db identityadapter.Connection, email string) string {
	t.Helper()
	salt := []byte("0123456789abcdef")
	hash := argon2.IDKey([]byte(testPassword), salt, 2, 19456, 1, 32)
	phc := "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash)
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
			w := loginPG(t, router, email, "wrong-password", "192.0.2."+[]string{"10", "11"}[i])
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
	good := loginPG(t, router, "session@example.test", testPassword, "192.0.2.20")
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
		if e = db.QueryRow(ctx, "SELECT token_hash FROM app.auth_sessions WHERE id=$1", response.SessionID).Scan(&actual); e != nil || hex.EncodeToString(actual) != hex.EncodeToString(want[:]) {
			t.Fatal("session persisted plaintext or incorrect hash")
		}
	})
	t.Run("known_device_account_exemption", func(t *testing.T) {
		if _, e := db.Exec(ctx, "INSERT INTO app.auth_account_attempts(identifier_normalized,failure_count,cooldown_until) VALUES('session@example.test',10,clock_timestamp()+interval '60 seconds') ON CONFLICT(identifier_normalized) DO UPDATE SET cooldown_until=excluded.cooldown_until"); e != nil {
			t.Fatal("cooldown fixture failed")
		}
		w := loginPG(t, router, "session@example.test", testPassword, "192.0.2.21", deviceCookie)
		if w.Code != 201 {
			t.Fatal("known owner locked out", w.Code)
		}
		if _, e := db.Exec(ctx, "UPDATE app.auth_account_attempts SET cooldown_until=clock_timestamp()-interval '1 second' WHERE identifier_normalized='session@example.test'"); e != nil {
			t.Fatal("fixture clear failed")
		}
	})
	t.Run("csrf_failure_then_logout_without_membership", func(t *testing.T) {
		w := requestPG(router, "POST", "/api/v1/auth/logout", "", "", "192.0.2.20", sessionCookie)
		if w.Code != 403 {
			t.Fatal("missing CSRF accepted", w.Code)
		}
		var revoked bool
		if e := db.QueryRow(ctx, "SELECT revoked_at IS NOT NULL FROM app.auth_sessions WHERE id=$1", response.SessionID).Scan(&revoked); e != nil || revoked {
			t.Fatal("CSRF denial mutated session")
		}
		w = requestPG(router, "POST", "/api/v1/auth/logout", "", response.CSRF, "192.0.2.20", sessionCookie)
		if w.Code != 204 {
			t.Fatal("membership-free logout failed", w.Code)
		}
		w = requestPG(router, "GET", "/api/v1/auth/session", "", "", "192.0.2.20", sessionCookie)
		if w.Code != 401 {
			t.Fatal("revocation not immediate", w.Code)
		}
	})
	t.Run("disabled_user_and_idle_and_absolute_denied", func(t *testing.T) {
		for i, mode := range []string{"disabled", "idle", "absolute"} {
			email := mode + "@example.test"
			uid := seedLoginPG(t, ctx, db, email)
			w := loginPG(t, router, email, testPassword, "198.51.100."+[]string{"10", "11", "12"}[i])
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
			w = requestPG(router, "GET", "/api/v1/auth/session", "", "", "198.51.100.10", c)
			if w.Code != 401 {
				t.Fatal(mode+" session admitted", w.Code)
			}
			if mode == "disabled" {
				w = loginPG(t, router, email, testPassword, "198.51.100.13")
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
	good := loginPG(t, router, "device@example.test", testPassword, "192.0.2.50")
	if good.Code != 201 {
		t.Fatal("known device fixture login refused", good.Code)
	}
	device := cookiePG(t, good, "__Host-osc_known_device")
	for i, ip := range []string{"192.0.2.51", "192.0.2.52", "192.0.2.53", "192.0.2.54", "192.0.2.55"} {
		w := loginPG(t, router, "device@example.test", "wrong-password", ip, device)
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
	w := loginPG(t, router, "device@example.test", testPassword, "192.0.2.56", device)
	if w.Code != 429 || waits != 0 || w.Header().Get("Retry-After") != "60" {
		t.Fatal("revoked device retained account exemption", w.Code, waits)
	}
	if tag, e := db.Exec(ctx, "UPDATE app.auth_account_attempts SET cooldown_until=clock_timestamp()-interval '1 second' WHERE identifier_normalized='device@example.test'"); e != nil || tag.RowsAffected() != 1 {
		t.Fatal("device cooldown reset failed", safeCode(e))
	}
	w = loginPG(t, router, "device@example.test", testPassword, "192.0.2.56", device)
	if w.Code != 201 || waits != 0 {
		t.Fatal("new login after cooldown refused", w.Code, waits)
	}
	newDevice := cookiePG(t, w, "__Host-osc_known_device")
	other := seedLoginPG(t, ctx, db, "otherdevice@example.test")
	if _, e := db.Exec(ctx, "INSERT INTO app.auth_account_attempts(identifier_normalized,failure_count,cooldown_until) VALUES('otherdevice@example.test',10,clock_timestamp()+interval '60 seconds')"); e != nil {
		t.Fatal("other account cooldown fixture failed", safeCode(e))
	}
	w = loginPG(t, router, "otherdevice@example.test", testPassword, "192.0.2.57", newDevice)
	if w.Code != 429 || waits != 0 {
		t.Fatal("different-user device retained account exemption", w.Code, waits)
	}
	_ = other
}
func waitLoginLocks(t *testing.T, ctx context.Context, db identityadapter.Connection, query string, count int) {
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
func TestLoginConcurrentPostgres(t *testing.T) {
	ctx, db, dsn, cfg := setupLoginPG(t)
	_, a := fixtureRouter(t, ctx, dsn, cfg)
	_, b := fixtureRouter(t, ctx, dsn, cfg)
	uid := seedLoginPG(t, ctx, db, "race@example.test")
	t.Run("two_actual_http_failures_add_two", func(t *testing.T) {
		tx, e := db.Begin(ctx)
		if e != nil {
			t.Fatal("gate transaction failed")
		}
		defer tx.Rollback(context.Background())
		if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('opensite:auth:'||$1,0))", "unknownrace@example.test"); e != nil {
			t.Fatal("gate lock failed")
		}
		ch := make(chan int, 2)
		c1, csrf1 := challengePG(t, a)
		c2, csrf2 := challengePG(t, b)
		body := `{"email":"unknownrace@example.test","password":"wrong-password"}`
		go func() { ch <- requestPG(a, "POST", "/api/v1/auth/login", body, csrf1, "203.0.113.30", c1).Code }()
		go func() { ch <- requestPG(b, "POST", "/api/v1/auth/login", body, csrf2, "203.0.113.30", c2).Code }()
		witness, e := identityadapter.Open(ctx, dsn)
		if e != nil {
			t.Fatal("witness unavailable")
		}
		defer witness.Close(context.Background())
		waitLoginLocks(t, ctx, witness, "%auth_record_failure%", 2)
		if e = tx.Commit(ctx); e != nil {
			t.Fatal("gate release failed")
		}
		if <-ch != 401 || <-ch != 401 {
			t.Fatal("concurrent failures response")
		}
		var x, y int
		if e = db.QueryRow(ctx, "SELECT a.failure_count,l.failure_count FROM app.auth_account_attempts a JOIN app.auth_login_attempts l USING(identifier_normalized) WHERE a.identifier_normalized='unknownrace@example.test'").Scan(&x, &y); e != nil || x != 2 || y != 2 {
			t.Fatal("lost concurrent failure", x, y)
		}
	})
	t.Run("same_old_session_only_one_successor", func(t *testing.T) {
		w := loginPG(t, a, "race@example.test", testPassword, "203.0.113.40")
		if w.Code != 201 {
			t.Fatal("old session fixture failed", w.Code)
		}
		old := cookiePG(t, w, "__Host-osc_admin_session")
		raw, _ := base64.RawURLEncoding.DecodeString(old.Value)
		hash := sha256.Sum256(raw)
		tx, e := db.Begin(ctx)
		if e != nil {
			t.Fatal("gate transaction failed")
		}
		defer tx.Rollback(context.Background())
		if _, e = tx.Exec(ctx, "SELECT id FROM app.auth_sessions WHERE token_hash=$1 FOR UPDATE", hash[:]); e != nil {
			t.Fatal("old session gate lock failed")
		}
		c1, s1 := challengePG(t, a)
		c2, s2 := challengePG(t, b)
		body := `{"email":"race@example.test","password":"` + testPassword + `"}`
		ch := make(chan int, 2)
		go func() { ch <- requestPG(a, "POST", "/api/v1/auth/login", body, s1, "203.0.113.41", old, c1).Code }()
		go func() { ch <- requestPG(b, "POST", "/api/v1/auth/login", body, s2, "203.0.113.42", old, c2).Code }()
		witness, e := identityadapter.Open(ctx, dsn)
		if e != nil {
			t.Fatal("witness unavailable")
		}
		defer witness.Close(context.Background())
		waitLoginLocks(t, ctx, witness, "%UPDATE app.auth_sessions s SET revoked_at%", 2)
		if e = tx.Commit(ctx); e != nil {
			t.Fatal("gate release failed")
		}
		results := []int{<-ch, <-ch}
		sort.Ints(results)
		if results[0] != 201 || results[1] != 401 {
			t.Fatal("rotation response", results)
		}
		var active, total int
		if e = db.QueryRow(ctx, "SELECT count(*) FILTER(WHERE revoked_at IS NULL),count(*) FROM app.auth_sessions WHERE user_id=$1", uid).Scan(&active, &total); e != nil || active != 1 || total != 2 {
			t.Fatal("rotation created extra successors", active, total)
		}
		w = requestPG(a, "GET", "/api/v1/auth/session", "", "", "203.0.113.40", old)
		if w.Code != 401 {
			t.Fatal("old token replay admitted", w.Code)
		}
	})
}
