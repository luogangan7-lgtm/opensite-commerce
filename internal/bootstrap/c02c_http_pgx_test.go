//go:build pgx

// Responsibilities: exercise production session/key HTTP admission and key
// management against disposable PostgreSQL with dedicated role logins.
// Dependencies: public identity/tenant modules, Router and pgxadapter; CI calls
// this gate. Credentials are random, SQL is parameterized, and rejected write
// authority must prevent the transaction callback from running.
package bootstrap_test

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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

const c02cHTTPOrigin = "https://admin.example.test"

// DecodeJSON reads after Router admission. This controlled reader creates the
// revocation window without replacing any authenticator, SQL or HTTP handler.
type c02cHTTPBody struct {
	reader io.Reader
	before func()
}

func (b *c02cHTTPBody) Read(p []byte) (int, error) {
	if b.before != nil {
		before := b.before
		b.before = nil
		before()
	}
	return b.reader.Read(p)
}

// This fixture uses libpq environment fields, never DSNs in subprocess argv.
// Installer diagnostics are discarded because they may repeat credentials.
func c02cHTTPPSQL(ctx context.Context, dsn, sql string, args ...string) error {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil || u.Hostname() == "" || u.Path == "" {
		return io.ErrUnexpectedEOF
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	credential, _ := u.User.Password()
	env := make([]string, 0, len(os.Environ())+8)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !strings.HasPrefix(key, "PG") && key != "DATABASE_URL" {
			env = append(env, item)
		}
	}
	fields := map[string]string{"PGHOST": u.Hostname(), "PGPORT": port, "PGUSER": u.User.Username(), "PGPASSWORD": credential, "PGDATABASE": strings.TrimPrefix(u.Path, "/"), "DATABASE_URL": dsn}
	for name, key := range map[string]string{"sslmode": "PGSSLMODE", "connect_timeout": "PGCONNECT_TIMEOUT", "sslrootcert": "PGSSLROOTCERT", "sslcert": "PGSSLCERT", "sslkey": "PGSSLKEY", "options": "PGOPTIONS"} {
		if value := u.Query().Get(name); value != "" {
			fields[key] = value
		}
	}
	for key, value := range fields {
		env = append(env, key+"="+value)
	}
	cmd := exec.CommandContext(ctx, "psql", append([]string{"-X", "-q", "-v", "ON_ERROR_STOP=1"}, args...)...)
	cmd.Env, cmd.Stdin = env, strings.NewReader(sql)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run()
}

func c02cHTTPRandom(t *testing.T, size int) []byte {
	t.Helper()
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		t.Fatal("HTTP fixture entropy unavailable")
	}
	return value
}

func c02cHTTPPGFixture(t *testing.T) (context.Context, *pgxadapter.IdentityRunner, string, string, string) {
	t.Helper()
	source := os.Getenv("DATABASE_URL")
	if source == "" {
		pgxtest.Skip(t, "NOT_RUN: disposable PostgreSQL DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	u, err := url.Parse(source)
	if err != nil {
		t.Fatal("HTTP fixture URL invalid")
	}
	suffix := hex.EncodeToString(c02cHTTPRandom(t, 12))
	database := "osc_c02c_http_" + suffix
	authRole, runtimeRole := database+"_auth", database+"_runtime"
	authCredential := base64.RawURLEncoding.EncodeToString(c02cHTTPRandom(t, 32))
	runtimeCredential := base64.RawURLEncoding.EncodeToString(c02cHTTPRandom(t, 32))
	if c02cHTTPPSQL(ctx, source, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()+";") != nil {
		t.Fatal("HTTP fixture database creation failed")
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if c02cHTTPPSQL(cleanup, source, "DROP DATABASE IF EXISTS "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE);") != nil {
			t.Error("HTTP fixture database cleanup failed")
		}
		for _, role := range []string{authRole, runtimeRole} {
			if c02cHTTPPSQL(cleanup, source, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize()+";") != nil {
				t.Error("HTTP fixture role cleanup failed")
			}
		}
	})
	u.Path = "/" + database
	fixtureDSN := u.String()
	migrations, err := filepath.Glob("../../migrations/[0-9][0-9][0-9]_*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("HTTP fixture migrations unavailable")
	}
	sort.Strings(migrations)
	maintenance, maintenanceErr := pgxadapter.OpenIdentity(ctx, source)
	if maintenanceErr != nil {
		t.Fatal("HTTP fixture maintenance connection unavailable")
	}
	defer maintenance.Close(context.Background())
	if initializationErr := pgxtest.WithFixtureInitialization(ctx, maintenance.InMaintenance, func(ctx context.Context) error {
		for _, path := range migrations {
			absolute, err := filepath.Abs(path)
			if err != nil {
				return errors.New("HTTP fixture migration path invalid")
			}
			if strings.HasSuffix(path, "_queue_authorization.sql") {
				installer := exec.CommandContext(ctx, "bash", "../../scripts/migrate_river.sh")
				installer.Env = append(os.Environ(), "DATABASE_URL="+fixtureDSN)
				installer.Stdout, installer.Stderr = io.Discard, io.Discard
				if installer.Run() != nil {
					return errors.New("HTTP fixture River installation failed")
				}
			}
			if c02cHTTPPSQL(ctx, fixtureDSN, "", "-1", "-f", absolute) != nil {
				return fmt.Errorf("HTTP fixture migration failed: %s", filepath.Base(path))
			}
		}
		for _, entry := range []struct{ role, credential, capability string }{{authRole, authCredential, "app_auth"}, {runtimeRole, runtimeCredential, "app_runtime"}} {
			sql := "CREATE ROLE " + pgx.Identifier{entry.role}.Sanitize() + " LOGIN INHERIT PASSWORD '" + entry.credential + "'; GRANT " + entry.capability + " TO " + pgx.Identifier{entry.role}.Sanitize() + ";"
			if c02cHTTPPSQL(ctx, source, sql) != nil {
				return errors.New("HTTP fixture role creation failed")
			}
		}
		return nil
	}); initializationErr != nil {
		t.Fatal("fixture initialization failed", initializationErr)
	}
	admin, err := pgxadapter.OpenIdentity(ctx, fixtureDSN)
	if err != nil {
		t.Fatal("HTTP fixture maintenance unavailable")
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	authURL, runtimeURL := *u, *u
	authURL.User = url.UserPassword(authRole, authCredential)
	runtimeURL.User = url.UserPassword(runtimeRole, runtimeCredential)
	return ctx, admin, fixtureDSN, authURL.String(), runtimeURL.String()
}

func TestC02cProductionHTTPPostgres(t *testing.T) {
	ctx, admin, _, authDSN, runtimeDSN := c02cHTTPPGFixture(t)
	execFixture := func(sql string, args ...any) {
		t.Helper()
		if err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, args...); return err }); err != nil {
			t.Fatal("HTTP fixture mutation failed")
		}
	}
	var user, target, other, session string
	rawSession := c02cHTTPRandom(t, 32)
	rawToken := base64.RawURLEncoding.EncodeToString(rawSession)
	digest := sha256.Sum256(rawSession)
	// Session lookup does not verify a password. Seed a random syntactically
	// valid PHC, never a static password or precomputed credential fixture.
	phc := "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(c02cHTTPRandom(t, 16)) + "$" + base64.RawStdEncoding.EncodeToString(c02cHTTPRandom(t, 32))
	if err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "INSERT INTO app.users(email,password_hash) VALUES($1,$2) RETURNING id::text", "http-owner@example.test", phc).Scan(&user); err != nil {
			return err
		}
		for _, out := range []*string{&target, &other} {
			if err := tx.QueryRow(ctx, "INSERT INTO app.tenants(name) VALUES($1) RETURNING id::text", "HTTP fixture tenant").Scan(out); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, "INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES($1,$3,'owner'),($2,$3,'admin')", target, other, user); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES($1,$2,clock_timestamp()+interval '1 hour') RETURNING id::text", user, digest[:]).Scan(&session)
	}); err != nil {
		t.Fatal("HTTP fixture authority seed failed")
	}
	cfg := identity.LoginConfig{Origin: c02cHTTPOrigin, SigningKey: c02cHTTPRandom(t, 32)}
	staff, err := identity.OpenLogin(ctx, authDSN, cfg)
	if err != nil {
		t.Fatal("HTTP fixture staff module unavailable")
	}
	t.Cleanup(func() { _ = staff.Close(context.Background()) })
	keys, err := tenant.Open(ctx, tenant.Config{DatabaseURL: authDSN})
	if err != nil {
		t.Fatal("HTTP fixture tenant module unavailable")
	}
	t.Cleanup(func() { _ = keys.Close(context.Background()) })
	staff.WithAuthorization(keys.Authorization())
	runtime, err := pgxadapter.OpenQueryRunner(ctx, runtimeDSN)
	if err != nil {
		t.Fatal("HTTP fixture runtime unavailable")
	}
	t.Cleanup(runtime.Close)
	router := platform.New(staff.Authenticator(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, route := range append(staff.Routes(), keys.Routes()...) {
		if err := router.Register(route.Spec, route.Handler); err != nil {
			t.Fatal("HTTP fixture route registration failed")
		}
	}
	witness := "/api/v1/t/{tenant_id}/c02c-witness"
	writes := 0
	var beforeWrite func()
	for _, method := range []string{"GET", "PUT"} {
		scope := "catalog:read"
		if method == "PUT" {
			scope = "catalog:write"
		}
		spec := platform.RouteSpec{OperationID: "c02cHTTPWitness" + method, Method: method, Pattern: witness, Access: platform.AdminOrAPIKey, Scope: scope}
		err := router.Register(spec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, err := platform.AuthorizedActor(r.Context())
			if err != nil || actor.ID == "" {
				t.Error("admitted HTTP actor missing")
				w.WriteHeader(500)
				return
			}
			if r.Method == "PUT" {
				if beforeWrite != nil {
					beforeWrite()
				}
				err := runtime.RunAuthorized(r.Context(), r.PathValue("tenant_id"), func(*db.Queries) error { writes++; return nil })
				if err != nil {
					platform.WriteProblem(w, r, err)
					return
				}
			}
			w.WriteHeader(204)
		}))
		if err != nil {
			t.Fatal("HTTP witness route registration failed")
		}
	}
	var beforeBodyRead func()
	request := func(method, path, body, bearer, csrf string, useCookie bool) *httptest.ResponseRecorder {
		reader := &c02cHTTPBody{reader: strings.NewReader(body), before: beforeBodyRead}
		q := httptest.NewRequest(method, path, reader)
		q = q.WithContext(ctx)
		q.RemoteAddr = "192.0.2.91:12345"
		q.Header.Set("Origin", c02cHTTPOrigin)
		q.Header.Set("Content-Type", "application/json")
		if useCookie {
			q.AddCookie(&http.Cookie{Name: "__Host-osc_admin_session", Value: rawToken})
		}
		if bearer != "" {
			q.Header.Set("Authorization", "Bearer "+bearer)
		}
		if csrf != "" {
			q.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, q)
		return w
	}
	expect := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("HTTP authorization status=%d want=%d", w.Code, status)
		}
	}
	path := "/api/v1/t/" + target + "/c02c-witness"
	keyPath := "/api/v1/t/" + target + "/api-keys"
	expect(request("GET", path, "", "", "", false), 401)
	expect(request("GET", path, "", "", "", true), 204)
	view := request("GET", "/api/v1/auth/session", "", "", "", true)
	expect(view, 200)
	var sessionView struct {
		CSRF string `json:"csrf_token"`
	}
	if json.Unmarshal(view.Body.Bytes(), &sessionView) != nil || sessionView.CSRF == "" {
		t.Fatal("production session CSRF absent")
	}
	expect(request("POST", keyPath, `{"scopes":["catalog:read"]}`, "", "", true), 403)
	created := request("POST", keyPath, `{"scopes":["catalog:read"]}`, "", sessionView.CSRF, true)
	expect(created, 201)
	if created.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("one-time key response cacheable")
	}
	var key struct {
		Value string `json:"key"`
		View  struct {
			ID string `json:"id"`
		} `json:"api_key"`
	}
	if json.Unmarshal(created.Body.Bytes(), &key) != nil || key.Value == "" || key.View.ID == "" {
		t.Fatal("created API key missing")
	}
	listed := request("GET", keyPath, "", "", "", true)
	expect(listed, 200)
	var list struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if json.Unmarshal(listed.Body.Bytes(), &list) != nil || len(list.Items) != 1 {
		t.Fatal("API key metadata list incorrect")
	}
	for _, item := range list.Items {
		for _, field := range []string{"key", "key_hash", "secret"} {
			if _, exists := item[field]; exists {
				t.Fatal("API key metadata leaked credential")
			}
		}
	}
	expect(request("GET", path, "", key.Value, "", false), 204)
	var lastUsed bool
	if err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT last_used_at IS NOT NULL AND last_used_at>=created_at AND last_used_at<=clock_timestamp() FROM app.api_keys WHERE tenant_id=$1 AND id=$2", target, key.View.ID).Scan(&lastUsed)
	}); err != nil || !lastUsed {
		t.Fatal("successful key use did not persist database-clock activity")
	}
	wrongValue := key.View.ID + "." + base64.RawURLEncoding.EncodeToString(c02cHTTPRandom(t, 32))
	expect(request("GET", path, "", wrongValue, "", false), 401)
	expect(request("GET", "/api/v1/t/"+other+"/c02c-witness", "", key.Value, "", false), 404)
	expect(request("PUT", path, "{}", key.Value, "", false), 403)
	expect(request("GET", keyPath, "", key.Value, "", false), 401)
	expect(request("PUT", path, "{}", "", "", true), 403)
	expect(request("PUT", path, "{}", "", sessionView.CSRF, true), 204)
	if writes != 1 {
		t.Fatal("authorized write callback missing")
	}
	beforeWrite = func() {
		execFixture("UPDATE app.tenant_memberships SET role='viewer' WHERE tenant_id=$1 AND user_id=$2", target, user)
	}
	// The transaction capability returns a boolean, so a lost authority is
	// sanitized as 401; ordinary narrowed-scope admission above returns 403.
	expect(request("PUT", path, "{}", "", sessionView.CSRF, true), 401)
	if writes != 1 {
		t.Fatal("role loss after HTTP admission reached write callback")
	}
	beforeWrite = nil
	execFixture("UPDATE app.tenant_memberships SET role='owner' WHERE tenant_id=$1 AND user_id=$2", target, user)
	expect(request("DELETE", keyPath+"/"+key.View.ID, "", "", sessionView.CSRF, true), 204)
	expect(request("GET", path, "", key.Value, "", false), 401)
	beforeBodyRead = func() { execFixture("UPDATE app.auth_sessions SET revoked_at=clock_timestamp() WHERE id=$1", session) }
	expect(request("POST", keyPath, `{"scopes":["catalog:read"]}`, "", sessionView.CSRF, true), 401)
	beforeBodyRead = nil
	var count int
	if err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM app.api_keys WHERE tenant_id=$1", target).Scan(&count)
	}); err != nil || count != 1 {
		t.Fatal("session revoked after admission still issued an API key")
	}
	execFixture("UPDATE app.auth_sessions SET revoked_at=NULL WHERE id=$1", session)
	execFixture("UPDATE app.auth_sessions SET created_at=clock_timestamp()-interval '2 hours',last_seen_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE id=$1", session)
	expect(request("GET", path, "", "", "", true), 401)
}
