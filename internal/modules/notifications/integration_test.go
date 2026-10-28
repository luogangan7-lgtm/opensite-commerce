//go:build pgx

// Responsibility: actual fanout, dedicated dispatch and admin recovery gates.
// Dependencies: public module/contract, pgxadapter and migrated expendable PG18.
// Invariants: fresh committed test tenants, test addresses, controlled roles;
// no raw connections and no business skip when DATABASE_URL is supplied.
package notifications_test

import (
	"context"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/outbox"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type fixture struct {
	ctx                                context.Context
	owner                              pgxadapter.Runner
	pool                               pgxadapter.QueueDB
	tenant, foreign, id, event, worker string
}

func notificationFixture(t *testing.T) fixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: expendable migrated PostgreSQL DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	owner, closeOwner, e := pgxadapter.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(closeOwner)
	pool, e := pgxadapter.OpenQueuePool(ctx, dsn, "app_outbox_worker")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(pool.Close)
	f := fixture{ctx: ctx, owner: owner, pool: pool}
	e = owner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if e := tx.QueryRow(ctx, `INSERT INTO app.tenants(name,settlement_currency) VALUES('notifications-go-gate','USD') RETURNING id::text`).Scan(&f.tenant); e != nil {
			return e
		}
		if e := tx.QueryRow(ctx, `INSERT INTO app.tenants(name,settlement_currency) VALUES('notifications-go-foreign','USD') RETURNING id::text`).Scan(&f.foreign); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, f.tenant); e != nil {
			return e
		}
		// The production order guard requires the controlled executor even for fixtures.
		if _, e := tx.Exec(ctx, `SET LOCAL ROLE app_orders_executor`); e != nil {
			return e
		}
		var order string
		if e := tx.QueryRow(ctx, `INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor) VALUES($1,uuidv7(),1,'d1d50d45-c431-5e93-8cd5-9b70112233aa',decode(repeat('01',32),'hex'),'{"email":"Buyer@safe.test","name":"<script>buyer</script>"}','USD',100,0,0,'{}',100) RETURNING id::text`, f.tenant).Scan(&order); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, `RESET ROLE`); e != nil {
			return e
		}
		if e := tx.QueryRow(ctx, `INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES($1,'order.paid',jsonb_build_object('order_id',$2::text,'recipient','injected@example.com'),'orders.events','notification-gate') RETURNING id::text`, f.tenant, order).Scan(&f.event); e != nil {
			return e
		}
		if e := tx.QueryRow(ctx, `SELECT id::text FROM app.notification_deliveries WHERE tenant_id=$1 AND source_event_id=$2`, f.tenant, f.event).Scan(&f.id); e != nil {
			return e
		}
		return tx.QueryRow(ctx, `SELECT uuidv7()::text`).Scan(&f.worker)
	})
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func (f fixture) maintenance(t *testing.T, fn func(context.Context, pgx.Tx) error) {
	t.Helper()
	if e := f.owner.InMaintenance(f.ctx, fn); e != nil {
		t.Fatal(e)
	}
}
func (f fixture) status(t *testing.T) (string, int32) {
	t.Helper()
	var status string
	var count int32
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status,attempt_count FROM app.notification_deliveries WHERE tenant_id=$1 AND id=$2`, f.tenant, f.id).Scan(&status, &count)
	})
	return status, count
}
func (f fixture) expire(t *testing.T) {
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE app.outbox_events SET lease_until=CASE WHEN status='processing' THEN clock_timestamp()-interval '1 second' ELSE NULL END,next_attempt_at=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND delivery_target='notifications.email'`, f.tenant)
		return e
	})
}
func (f fixture) admin() notifications.TenantRunner {
	return func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
		return f.owner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, e := tx.Exec(ctx, `SET LOCAL ROLE app_runtime`); e != nil {
				return e
			}
			return fn(ctx, tx)
		})
	}
}
func TestNotificationFanoutReplayAndDedicatedClaim(t *testing.T) {
	f := notificationFixture(t)
	s := notifications.OutboxStore(f.pool)
	fake := &notifications.Fake{}
	m := notifications.NewWorker(f.pool, fake)
	leases, e := s.Claim(f.ctx, f.tenant, f.worker, 1, 30)
	if e != nil || len(leases) != 1 {
		t.Fatal("NOTIFICATION_DEDICATED_CLAIM", e, len(leases))
	}
	if leases[0].Delivery.Target != "notifications.email" {
		t.Fatal("NOTIFICATION_TARGET")
	}
	if e = m.Process(f.ctx, leases[0]); e != nil {
		t.Fatal(e)
	}
	if status, _ := f.status(t); status != "sent" {
		t.Fatal("NOTIFICATION_SENT", status)
	}
	// Crash between sent persistence and outbox ACK: takeover must not resend.
	f.expire(t)
	leases, e = s.Claim(f.ctx, f.tenant, f.worker, 1, 30)
	if e != nil || len(leases) != 1 {
		t.Fatal(e, len(leases))
	}
	if e = m.Process(f.ctx, leases[0]); e != nil {
		t.Fatal(e)
	}
	if ok, e := s.Complete(f.ctx, leases[0]); e != nil || !ok {
		t.Fatal(e, ok)
	}
	sent := fake.Snapshot()
	if len(sent) != 1 || sent[0].Recipient != "buyer@safe.test" {
		t.Fatal("NOTIFICATION_FROZEN_RECIPIENT", sent)
	}
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, f.tenant); e != nil {
			return e
		}
		tag, e := tx.Exec(ctx, `INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) SELECT tenant_id,event_type,payload,delivery_target,dedup_key FROM app.outbox_events WHERE tenant_id=$1 AND id=$2 ON CONFLICT(tenant_id,delivery_target,dedup_key) DO NOTHING`, f.tenant, f.event)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 0 {
			t.Fatal("NOTIFICATION_DUPLICATE_SOURCE", tag.RowsAffected())
		}
		var n int
		if e := tx.QueryRow(ctx, `SELECT count(*) FROM app.notification_deliveries WHERE tenant_id=$1 AND source_event_id=$2`, f.tenant, f.event).Scan(&n); e != nil {
			return e
		}
		if n != 1 {
			t.Fatal("NOTIFICATION_SOURCE_DEDUP", n)
		}
		var status string
		if e := tx.QueryRow(ctx, `SELECT status FROM app.outbox_events WHERE tenant_id=$1 AND id=$2`, f.tenant, f.event).Scan(&status); e != nil {
			return e
		}
		if status != "pending" {
			t.Fatal("NOTIFICATION_OTHER_TARGET_STOLEN", status)
		}
		return nil
	})
}
func TestNotificationTenFailuresDLQRedriveIsolation(t *testing.T) {
	f := notificationFixture(t)
	s := notifications.OutboxStore(f.pool)
	fake := &notifications.Fake{}
	fake.FailNext(10)
	m := notifications.NewWorker(f.pool, fake)
	d := outbox.Dispatcher{Store: s, Sender: m, Config: outbox.Config{WorkerID: f.worker, LeaseSeconds: 30, BaseRetrySeconds: 1, MaxRetrySeconds: 2}}
	for i := 1; i <= 10; i++ {
		result, e := d.Dispatch(f.ctx, f.tenant, 1)
		if e != nil || result.Failed != 1 {
			t.Fatal("NOTIFICATION_RETRY", i, result, e)
		}
		status, n := f.status(t)
		if n != int32(i) {
			t.Fatal("NOTIFICATION_ATTEMPT", n, i)
		}
		want := "failed"
		if i == 10 {
			want = "dead_letter"
		}
		if status != want {
			t.Fatal("NOTIFICATION_RETRY_STATUS", status, want)
		}
		if i < 10 {
			f.expire(t)
		}
	}
	if result, e := d.Dispatch(f.ctx, f.tenant, 1); e != nil || result.Claimed != 0 {
		t.Fatal("NOTIFICATION_ATTEMPT_CAP", result, e)
	}
	a := notifications.New(f.admin())
	foreign, e := a.ListFailures(f.ctx, f.foreign, 100)
	if e != nil || len(foreign) != 0 {
		t.Fatal("NOTIFICATION_TENANT_READ", foreign, e)
	}
	if _, e = a.Redrive(f.ctx, f.foreign, f.id, "5b5cda6b-15cc-5e9e-9a04-c28e2b4c565d"); e == nil {
		t.Fatal("NOTIFICATION_TENANT_REDRIVE")
	} else {
		var app *apperrors.Error
		if !errors.As(e, &app) || app.Status != 404 {
			t.Fatal("NOTIFICATION_TENANT_404", e)
		}
	}
	failures, e := a.ListFailures(f.ctx, f.tenant, 100)
	if e != nil || len(failures) != 1 || failures[0].Status != "dead_letter" {
		t.Fatal("NOTIFICATION_FAILURES", failures, e)
	}
	one, e := a.Redrive(f.ctx, f.tenant, f.id, "b94ed2a2-aa70-5850-b3bc-362c81565254")
	if e != nil {
		t.Fatal(e)
	}
	two, e := a.Redrive(f.ctx, f.tenant, f.id, "b94ed2a2-aa70-5850-b3bc-362c81565254")
	if e != nil || one != two || one.Token == "" || one.Generation != 2 {
		t.Fatal("NOTIFICATION_REDRIVE_IDEMPOTENCY", one, two, e)
	}
	if result, e := d.Dispatch(f.ctx, f.tenant, 1); e != nil || result.Completed != 1 {
		t.Fatal("NOTIFICATION_REDRIVE_SUCCESS", result, e)
	}
	if status, _ := f.status(t); status != "sent" || len(fake.Snapshot()) != 1 {
		t.Fatal("NOTIFICATION_REDRIVE_SENT", status)
	}
}
func TestNotificationUncertainNeverResends(t *testing.T) {
	f := notificationFixture(t)
	fake := &notifications.Fake{}
	fake.AmbiguousNext()
	m := notifications.NewWorker(f.pool, fake)
	d := outbox.Dispatcher{Store: notifications.OutboxStore(f.pool), Sender: m, Config: outbox.Config{WorkerID: f.worker, LeaseSeconds: 30, BaseRetrySeconds: 1, MaxRetrySeconds: 2}}
	if result, e := d.Dispatch(f.ctx, f.tenant, 1); e != nil || result.Completed != 1 {
		t.Fatal(result, e)
	}
	if status, _ := f.status(t); status != "uncertain" {
		t.Fatal("NOTIFICATION_UNCERTAIN", status)
	}
	if result, e := d.Dispatch(f.ctx, f.tenant, 1); e != nil || result.Claimed != 0 || len(fake.Snapshot()) != 1 {
		t.Fatal("NOTIFICATION_UNCERTAIN_AUTORETRY", result, e)
	}
	if _, e := notifications.New(f.admin()).Redrive(f.ctx, f.tenant, f.id, "a0f8c299-7c8d-5568-83e4-9ff653ae5ab3"); e == nil {
		t.Fatal("NOTIFICATION_UNCERTAIN_REDRIVE")
	} else {
		var app *apperrors.Error
		if !errors.As(e, &app) || app.Status != 409 {
			t.Fatal("NOTIFICATION_UNCERTAIN_409", e)
		}
	}
}
