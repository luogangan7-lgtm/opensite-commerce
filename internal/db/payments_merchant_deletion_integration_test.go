//go:build pgx

// Responsibility: prove default/fake settlement reaches Tenant SQL admission.
// Dependencies: real Payments/Checkout fixture and migrated PG18 CI database.
// Invariants: unknown/unsettled block both phases; settled fake is test-only;
// a duplicate capture preserves every financial and outbox witness.
package db_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	paymentcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
	tenant "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant/contract"
)

type merchantSettlementFake paymentcontract.SettlementOutcome

func (f merchantSettlementFake) Settlement(context.Context, *db.Queries, string) (paymentcontract.SettlementOutcome, error) {
	return paymentcontract.SettlementOutcome(f), nil
}

func TestMerchantDeletionAdmissionAndMoneyReplay(t *testing.T) {
	p := newPaymentFactFixture(t)
	p.receive(t, "mdel04-capture", payment.StatusCaptured, p.start.AmountMinor)
	before := p.witness(t)
	p.receive(t, "mdel04-capture", payment.StatusCaptured, p.start.AmountMinor)
	if after := p.witness(t); after != before {
		t.Fatalf("MDEL04_DUPLICATE_MONEY_EFFECT before=%+v after=%+v", before, after)
	}
	for _, tc := range []struct {
		name     string
		provider paymentcontract.ProviderSettlement
		blocked  bool
	}{
		{"default", nil, true},
		{"unsettled", merchantSettlementFake(paymentcontract.SettlementUnsettled), true},
		{"settled-test-only", merchantSettlementFake(paymentcontract.SettlementSettled), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", p.f.tenant); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, "SELECT app.tenant_lifecycle_transition_fence($1)", p.f.tenant); err != nil {
					return err
				}
				items, err := payments.NewDeletionObligations(tc.provider).DeletionObligations(ctx, db.New(tx), p.f.tenant)
				if err != nil {
					return err
				}
				counts := tenant.ObligationCounts(items)
				// Other module authorities are supplied ONLY by this test fixture;
				// no production composition may assume their zero witnesses.
				for _, c := range []tenant.ObligationCategory{tenant.PaidUnfulfilled, tenant.FulfillmentReturn, tenant.CheckoutCapture, tenant.LegalHold} {
					counts[c] = 0
				}
				if counts[tenant.PaymentRefund] != 0 || counts[tenant.RefundReconciliation] != 0 {
					t.Fatal("MDEL04_INTERNAL_SETTLED_WITNESS")
				}
				encoded, err := json.Marshal(counts)
				if err != nil {
					return err
				}
				var result []byte
				request := uuid.NewString()
				var user, session string
				if err := tx.QueryRow(ctx, "INSERT INTO app.users(email,password_hash) VALUES($1,'fixture') RETURNING id::text", "mdel04-"+request+"@invalid.test").Scan(&user); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, "INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')", p.f.tenant, user); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx, "INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES($1,sha256($2::bytea),clock_timestamp()+interval '1 day') RETURNING id::text", user, []byte(request)).Scan(&session); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, "SELECT set_config('app.session_idle_seconds','1800',true)"); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx, "SELECT app.tenant_lifecycle_transition($1,$2,$3,$4,'request',$5,sha256('request'::bytea),0,$6::jsonb)", p.f.tenant, request, user, session, uuid.NewString(), encoded).Scan(&result); err != nil {
					return err
				}
				var admission struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(result, &admission); err != nil {
					return err
				}
				if (admission.Code == "tenant_deletion_blocked") != tc.blocked {
					t.Fatalf("MDEL04_REQUEST_GATE: %s", result)
				}
				// Execute exercises the same count map with the real Tenant worker
				// boundary with the same Payments witness used for request admission.
				if _, err := tx.Exec(ctx, "UPDATE app.tenant_lifecycles SET state='cooling_off',phase='none',request_id=$2,requester_id=$3,epoch=1,requested_at=clock_timestamp()-interval '721 hours',execute_after=clock_timestamp()-interval '1 hour' WHERE tenant_id=$1", p.f.tenant, request, user); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx, "SELECT app.tenant_lifecycle_transition($1,$2,NULL,NULL,'execute',NULL,NULL,1,$3::jsonb)", p.f.tenant, request, encoded).Scan(&result); err != nil {
					return err
				}
				var state struct {
					State string `json:"state"`
				}
				if err := json.Unmarshal(result, &state); err != nil {
					return err
				}
				if (state.State == "failed") != tc.blocked || !tc.blocked && state.State != "executing" {
					t.Fatalf("MDEL04_EXECUTION_GATE: %s", result)
				}
				// Always restore fixture state, without ever enabling deletion routes.
				_, err = tx.Exec(ctx, "UPDATE app.tenant_lifecycles SET state='active',phase='none',next_retry_at=NULL WHERE tenant_id=$1", p.f.tenant)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	// The same authenticated capture can replay while cooling; it cannot create
	// another journal. Execution fences it before any financial effect.
	setState := func(state string) {
		t.Helper()
		err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "UPDATE app.tenant_lifecycles SET state=$2,phase=CASE WHEN $2='executing' THEN 'executing' ELSE 'none' END WHERE tenant_id=$1", p.f.tenant, state)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	setState("cooling_off")
	p.receive(t, "mdel04-capture", payment.StatusCaptured, p.start.AmountMinor)
	if p.witness(t) != before {
		t.Fatal("MDEL04_COOLING_REPLAY_MONEY_EFFECT")
	}
	setState("executing")
	raw := p.raw("mdel04-late", payment.StatusCaptured, p.start.AmountMinor)
	if _, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); err == nil {
		t.Fatal("MDEL04_EXECUTION_INGRESS")
	}
	if p.witness(t) != before {
		t.Fatal("MDEL04_EXECUTION_MONEY_EFFECT")
	}
	setState("active")
}
