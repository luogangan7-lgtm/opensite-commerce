//go:build pgx

// Responsibility: exercise refundReturn with the production coordinator LOGIN.
// Dependencies: disposable PG, quote paid-order fixture, staff auth and Fake PSP.
// Caller: foundation pgx gate. Invariant: authenticated refund and replay produce
// one provider refund and one journal without borrowing runtime privileges.
package bootstrap_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func TestReturnsCoordinatorAuthenticatedRefundPG(t *testing.T) {
	f := quoteOrdersDB(t, time.Hour)
	paid, err := f.convertAndPay(t, f.buyerCtx, f.quote.ID, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	raw := c02cHTTPRandom(t, 32)
	hash := sha256.Sum256(raw)
	var version int64
	var line string
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES($1,$2,clock_timestamp()+interval '1 hour')`, f.staff, hash[:]); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT o.version,l.id FROM app.orders o JOIN app.order_lines l ON l.tenant_id=o.tenant_id AND l.order_id=o.id WHERE o.tenant_id=$1 AND o.id=$2`, f.tenant, paid.OrderID).Scan(&version, &line)
	})
	const origin = "https://returns.example.test"
	for key, value := range map[string]string{
		"OPENSITE_DATABASE_URL":         loginDSN(t, f.ctx, f.runner.Runner, os.Getenv("DATABASE_URL"), "retgrant_runtime_", "app_runtime"),
		"OPENSITE_AUTH_DATABASE_URL":    loginDSN(t, f.ctx, f.runner.Runner, os.Getenv("DATABASE_URL"), "retgrant_auth_", "app_auth"),
		"OPENSITE_RETURNS_DATABASE_URL": loginDSN(t, f.ctx, f.runner.Runner, os.Getenv("DATABASE_URL"), "retgrant_service_", "app_returns_service"),
		"OSC_ADMIN_ORIGIN":              origin, "OSC_AUTH_SIGNING_KEY_HEX": hex.EncodeToString(quoteRandom(t)),
		"OPENSITE_IDEMPOTENCY_ROOT": t.TempDir(),
	} {
		t.Setenv(key, value)
	}
	cfg, closeRuntime, err := bootstrap.FromEnvironment(f.ctx, bootstrap.Config{Payment: f.fake})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeRuntime)
	if cfg.ReturnsCoordinator.Runner == nil || cfg.Returns.Service == nil {
		t.Fatal("RETURNS_AUTHORITY_RED: production coordinator not composed")
	}
	called := false
	if err := cfg.ReturnsCoordinator.Runner.InTenant(f.ctx, f.tenant, func(context.Context, pgx.Tx) error { called = true; return nil }); err == nil || called {
		t.Fatal("unauthenticated coordinator callback admitted")
	}
	server, err := bootstrap.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	csrf := ""
	request := func(method, path, body, key string, version int64) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, origin+path, strings.NewReader(body)).WithContext(f.ctx)
		r.AddCookie(&http.Cookie{Name: "__Host-osc_admin_session", Value: base64.RawURLEncoding.EncodeToString(raw)})
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("If-Match", fmt.Sprintf(`"%d"`, version))
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, r)
		return w
	}
	session := request("GET", "/api/v1/auth/session", "", "", 0)
	var challenge struct {
		Token string `json:"csrf_token"`
	}
	if session.Code != 200 || json.Unmarshal(session.Body.Bytes(), &challenge) != nil || challenge.Token == "" {
		t.Fatalf("staff session: %d %s", session.Code, session.Body.String())
	}
	csrf = challenge.Token
	created := request("POST", "/api/v1/t/"+f.tenant+"/orders/"+paid.OrderID+"/returns", fmt.Sprintf(`{"kind":"unshipped_cancel","reason":"CI coordinator regression","lines":[{"order_line_id":%q,"quantity":2}]}`, line), uuid.NewString(), version)
	var receipt httpapi.ReturnRecord
	if created.Code != 201 || json.Unmarshal(created.Body.Bytes(), &receipt) != nil {
		t.Fatalf("createReturn: %d %s", created.Code, created.Body.String())
	}
	path := "/api/v1/t/" + f.tenant + "/returns/" + receipt.Id.String() + "/refund"
	key := uuid.NewString()
	for attempt := range 2 {
		w := request("POST", path, `{}`, key, receipt.OrderVersion)
		var result httpapi.ReturnRecord
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Status != "refunded" || result.RefundMinor != "270" {
			t.Fatalf("RETURNS_AUTHORITY_RED: refundReturn attempt=%d status=%d body=%s", attempt, w.Code, w.Body.String())
		}
	}
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		var journals, refunded int64
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=$1 AND source_type='return' AND source_id=$2),refunded_minor FROM app.orders WHERE tenant_id=$1 AND id=$3`, f.tenant, receipt.Id.String(), paid.OrderID).Scan(&journals, &refunded); err != nil {
			return err
		}
		if journals != 1 || refunded != 270 || len(f.fake.Events()) != 3 {
			return fmt.Errorf("refund conservation: journals=%d refunded=%d provider events=%d", journals, refunded, len(f.fake.Events()))
		}
		return nil
	})
}
