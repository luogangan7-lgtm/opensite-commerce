//go:build pgx

// Responsibility: exercise retained refund cases and recovery against production SQL.
// Dependencies: migrated PG and mapped payment fixtures; caller: CI pgx lanes.
// Invariants: replay commits one compensation; case history is immutable and tenant-bound.
package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func TestRefundCaseRecovery(t *testing.T) {
	p := newPaymentFactFixture(t)
	p.receive(t, "capture", payment.StatusCaptured, p.start.AmountMinor)
	var body map[string]any
	_ = json.Unmarshal(p.raw("recovery-success", payment.StatusPartiallyRefunded, 1), &body)
	body["refund_id"] = "recovery-refund"
	raw, _ := json.Marshal(body)
	if _, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("retained authenticated terminal fact"))
	fact := map[string]any{"account_id": p.account, "refund_id": "recovery-refund", "provider_ref": p.start.Payment.ProviderRef,
		"event_id": "recovery-terminal", "payload_hash": hex.EncodeToString(hash[:]), "reason": "unmatched_refund"}
	var caseID string
	retain := func() error {
		return p.runTerminal(p.f.ctx, p.account, func(ctx context.Context, tx pgx.Tx, _ string, _ time.Time) error {
			b, _ := json.Marshal(fact)
			return tx.QueryRow(ctx, "SELECT app.payments_retain_refund_case($1::jsonb)::text", b).Scan(&caseID)
		})
	}
	if err := retain(); err != nil {
		t.Fatalf("C19D3_RETAIN_RED: %v", err)
	}
	first := caseID
	if err := retain(); err != nil || caseID != first {
		t.Fatalf("case dedup: %s %s %v", first, caseID, err)
	}
	read := func(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
		return p.f.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET TRANSACTION READ ONLY"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", tenant); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
				return err
			}
			return fn(db.New(tx))
		})
	}
	router := httpapi.New(fulfillmentHTTPTestAuth{tenant: p.f.tenant}, nil)
	for _, route := range payments.NewReconciliationHTTP(read).Routes() {
		if err := router.Handle(route); err != nil {
			t.Fatal(err)
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/t/"+p.f.tenant+"/payments/refund-cases", nil))
	var page struct {
		Count int `json:"open_count"`
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || page.Count != 1 || len(page.Items) != 1 || page.Items[0].ID != caseID {
		t.Fatalf("C19D3_STAFF_READ_RED status=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}
	before := p.witness(t)
	abort := errors.New("crash before case/financial commit")
	replay := func(crash bool) error {
		return p.runTerminal(p.f.ctx, p.account, func(ctx context.Context, tx pgx.Tx, tenant string, _ time.Time) error {
			b, _ := json.Marshal(map[string]any{"tenant_id": tenant, "provider": "fake", "account_id": p.account,
				"refund_id": "recovery-refund", "provider_ref": p.start.Payment.ProviderRef,
				"event_id": "recovery-terminal", "payload_hash": hex.EncodeToString(hash[:]), "status": "failed",
				"amount_minor": 1, "currency": p.start.Currency, "order_version": 0})
			_, err := tx.Exec(ctx, "SELECT app.payments_replay_refund_case($1::uuid,$2::jsonb)", caseID, b)
			if err == nil && crash {
				return abort
			}
			return err
		})
	}
	if err := replay(true); !errors.Is(err, abort) || p.witness(t) != before {
		t.Fatalf("C19D3_ATOMIC_CASE_RED %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := replay(false); err != nil {
			t.Fatalf("C19D3_REPLAY_RED iteration=%d: %v", i, err)
		}
	}
	after := p.witness(t)
	if before.refunded != 1 || after.refunded != 0 || after.checkout.cash != before.checkout.cash+1 {
		t.Fatalf("C19D3_DOUBLE_COMPENSATION_RED before=%+v after=%+v", before, after)
	}
	err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		var cases, links, resolutions, reversals int
		if err := tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM app.payment_refund_cases WHERE tenant_id=$1),
 (SELECT count(*) FROM app.payment_refund_case_deliveries WHERE tenant_id=$1),
 (SELECT count(*) FROM app.payment_refund_case_resolutions WHERE tenant_id=$1 AND outcome='resolved_compensated'),
 (SELECT count(*) FROM app.payment_refund_reversals WHERE tenant_id=$1)`, p.f.tenant).Scan(&cases, &links, &resolutions, &reversals); err != nil {
			return err
		}
		if cases != 1 || links != 1 || resolutions != 1 || reversals != 1 {
			t.Fatalf("C19D3_EXACTLY_ONCE_RED cases=%d links=%d resolutions=%d reversals=%d", cases, links, resolutions, reversals)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fact["payload_hash"] = hex.EncodeToString(make([]byte, 32))
	if err := retain(); err == nil {
		t.Fatal("changed retained payload accepted")
	}
}

func TestRefundCaseOperatorAndAudit(t *testing.T) {
	p := newPaymentFactFixture(t)
	actor, credential := uuid.NewString(), uuid.NewString()
	h := sha256.Sum256([]byte(actor))
	if err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{"INSERT INTO app.users(id,email,password_hash) VALUES($1,$2,'fixture')", []any{actor, actor + "@example.invalid"}},
			{"INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')", []any{p.f.tenant, actor}},
			{"INSERT INTO app.auth_sessions(id,user_id,token_hash,expires_at) VALUES($1,$2,$3,clock_timestamp()+interval '1 hour')", []any{credential, actor, h[:]}},
		} {
			if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var cid string
	if err := p.runTerminal(p.f.ctx, p.account, func(ctx context.Context, tx pgx.Tx, _ string, _ time.Time) error {
		raw, _ := json.Marshal(map[string]string{"account_id": p.account, "refund_id": "operator-refund", "provider_ref": p.start.Payment.ProviderRef, "event_id": "operator-case", "payload_hash": hex.EncodeToString(h[:]), "reason": "unmatched_refund"})
		return tx.QueryRow(ctx, "SELECT app.payments_retain_refund_case($1::jsonb)::text", raw).Scan(&cid)
	}); err != nil {
		t.Fatal(err)
	}
	resolution := payments.OperatorResolution{UserID: actor, Kind: "session", CredentialID: credential, Reason: "external_verified", EvidenceHash: hex.EncodeToString(h[:])}
	resolve := func() error {
		return p.runTerminal(p.f.ctx, p.account, func(ctx context.Context, tx pgx.Tx, _ string, _ time.Time) error {
			return payments.ResolveRefundCase(ctx, tx, cid, resolution)
		})
	}
	w := p.witness(t)
	for i := 0; i < 2; i++ {
		if err := resolve(); err != nil {
			t.Fatalf("C19D3_OPERATOR_RED %v", err)
		}
	}
	if p.witness(t) != w {
		t.Fatal("operator resolution changed financial/Inbox facts")
	}
	for _, statement := range []string{
		"UPDATE app.payment_refund_cases SET reason=reason WHERE tenant_id=$1",
		"DELETE FROM app.payment_refund_case_resolutions WHERE tenant_id=$1",
		"UPDATE app.payment_refund_case_deliveries SET created_at=created_at WHERE tenant_id=$1",
	} {
		err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", p.f.tenant); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, statement, p.f.tenant)
			return err
		})
		var pg interface{ SQLState() string }
		if !errors.As(err, &pg) || pg.SQLState() != "23514" {
			t.Fatalf("C19D3_AUDIT_IMMUTABLE_RED %v", err)
		}
	}
	if err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE app.tenant_memberships SET role='viewer' WHERE tenant_id=$1 AND user_id=$2", p.f.tenant, actor); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", uuid.NewString())
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
			return err
		}
		var raw []byte
		if err = tx.QueryRow(ctx, "SELECT app.payments_open_refund_cases(NULL)").Scan(&raw); err != nil {
			return err
		}
		var out struct {
			Count int `json:"open_count"`
		}
		if err = json.Unmarshal(raw, &out); err != nil {
			return err
		}
		if out.Count != 0 {
			return fmt.Errorf("C19D3_FOREIGN_READ_RED %s", raw)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := resolve(); err == nil {
		t.Fatal("C19D3_STALE_AUTHORITY_RED viewer resolved case")
	}
	resolution.Reason = ""
	if err := resolve(); err == nil {
		t.Fatal("operator resolved without reason")
	}
}
