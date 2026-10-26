//go:build pgx

// Responsibility: distinguish staff decisions from independently verified refund facts.
// Dependencies: production payment/auth fixtures and migrated PG; caller: CI pgx lanes.
// Invariants: both histories survive either order, replay, and transaction rollback.
package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
)

func TestRefundResolutionIndependentFacts(t *testing.T) {
	for _, compensated := range []bool{false, true} {
		for _, terminalFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("compensated=%t/terminalFirst=%t", compensated, terminalFirst), func(t *testing.T) {
				p := newPaymentFactFixture(t)
				p.receive(t, "capture", payment.StatusCaptured, p.start.AmountMinor)
				if compensated {
					var body map[string]any
					if err := json.Unmarshal(p.raw("audit-success", payment.StatusPartiallyRefunded, 1), &body); err != nil {
						t.Fatal(err)
					}
					body["refund_id"] = "audit-refund"
					raw, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); err != nil {
						t.Fatal(err)
					}
				}
				actor, credential := uuid.NewString(), uuid.NewString()
				h := sha256.Sum256([]byte(actor))
				if err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
					if _, err := tx.Exec(ctx, "INSERT INTO app.users(id,email,password_hash) VALUES($1,$2,'fixture')", actor, actor+"@example.invalid"); err != nil {
						return err
					}
					if _, err := tx.Exec(ctx, "INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')", p.f.tenant, actor); err != nil {
						return err
					}
					_, err := tx.Exec(ctx, "INSERT INTO app.auth_sessions(id,user_id,token_hash,expires_at) VALUES($1,$2,$3,clock_timestamp()+interval '1 hour')", credential, actor, h[:])
					return err
				}); err != nil {
					t.Fatal(err)
				}
				fact, err := json.Marshal(map[string]any{"tenant_id": p.f.tenant, "provider": "fake", "account_id": p.account,
					"refund_id": "audit-refund", "provider_ref": p.start.Payment.ProviderRef, "event_id": "audit-terminal",
					"payload_hash": hex.EncodeToString(h[:]), "reason": "unmatched_refund", "status": "failed",
					"amount_minor": 1, "currency": p.start.Currency, "order_version": 0})
				if err != nil {
					t.Fatal(err)
				}
				var cid string
				if err := p.runTerminal(p.f.ctx, p.account, func(ctx context.Context, tx pgx.Tx, _ string, _ time.Time) error {
					return tx.QueryRow(ctx, "SELECT app.payments_retain_refund_case($1::jsonb)::text", fact).Scan(&cid)
				}); err != nil {
					t.Fatal(err)
				}
				resolution := payments.OperatorResolution{UserID: actor, Kind: "session", CredentialID: credential, Reason: "no_posting_required", EvidenceHash: hex.EncodeToString(h[:])}
				resolve := func() error {
					return p.runTerminal(p.f.ctx, p.account, func(ctx context.Context, tx pgx.Tx, _ string, _ time.Time) error {
						return payments.ResolveRefundCase(ctx, tx, cid, resolution)
					})
				}
				abort := errors.New("audit append rolled back with financial facts")
				replay := func(rollback bool) error {
					return p.runTerminal(p.f.ctx, p.account, func(ctx context.Context, tx pgx.Tx, _ string, _ time.Time) error {
						if _, err := tx.Exec(ctx, "SELECT app.payments_replay_refund_case($1::uuid,$2::jsonb)", cid, fact); err != nil {
							return err
						}
						if rollback {
							return abort
						}
						return nil
						})
				}
				manualWitness := func() string {
					t.Helper()
					var witness string
					if err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
						return tx.QueryRow(ctx, "SELECT coalesce(string_agg(row_to_json(r)::text,''),'') FROM app.payment_refund_case_resolutions r WHERE tenant_id=$1 AND case_id=$2 AND reason<>'verified_terminal'", p.f.tenant, cid).Scan(&witness)
					}); err != nil {
						t.Fatal(err)
					}
					return witness
				}
				if !terminalFirst {
					if err := resolve(); err != nil {
						t.Fatal(err)
					}
				}
				before, manual := p.witness(t), manualWitness()
				if err := replay(true); !errors.Is(err, abort) || p.witness(t) != before || manualWitness() != manual {
					t.Fatalf("B369_ATOMIC_AUDIT_RED: %v", err)
				}
				for range 2 {
					if err := replay(false); err != nil {
						t.Fatal(err)
					}
				}
				if !terminalFirst && manualWitness() != manual {
					t.Fatal("B369_MANUAL_REWRITTEN_RED")
				}
				for range 2 {
					if err := resolve(); err != nil {
						t.Fatalf("B369_MANUAL_REPLAY_RED: %v", err)
					}
				}
				if err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
					var manual, terminal, reversals int
					var valid bool
					if err := tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE reason<>'verified_terminal'),
 count(*) FILTER(WHERE reason='verified_terminal'),
 bool_and(CASE WHEN reason='verified_terminal' THEN terminal_refund_id='audit-refund' AND verifier IS NULL
 ELSE verifier=$3 AND evidence_hash=$4 AND reason='no_posting_required' END),
 (SELECT count(*) FROM app.payment_refund_reversals WHERE tenant_id=$1)
 FROM app.payment_refund_case_resolutions WHERE tenant_id=$1 AND case_id=$2`, p.f.tenant, cid, actor, h[:]).Scan(&manual, &terminal, &valid, &reversals); err != nil {
						return err
					}
					want := 0
					if compensated {
						want = 1
					}
					if manual != 1 || terminal != 1 || !valid || reversals != want {
						return fmt.Errorf("B369_INDEPENDENT_AUDIT_RED manual=%d terminal=%d valid=%t reversals=%d want=%d", manual, terminal, valid, reversals, want)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				resolution.Reason = "external_verified"
				var state interface{ SQLState() string }
				if err := resolve(); !errors.As(err, &state) || state.SQLState() != "23505" {
					t.Fatalf("B369_MANUAL_CONFLICT_RED: %v", err)
				}
			})
		}
	}
}
