//go:build pgx

// Responsibility: production sqlc/011 worker role, commit separation and fencing.
// Dependencies: disposable migrated PG18 DATABASE_URL and pgx. Caller: PG gate;
// OUTBOX_REQUIRE_PG=1 turns missing DB into failure. Committed fixtures are scoped
// to unique tenants, cleaned by admin; expiry is admin fixture update, never sleep.
package db_test

import (
	"context"
	"errors"
	outbox "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/outbox"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func outboxPGFixture(t *testing.T) (context.Context, *pgxpool.Pool, outbox.PGStore, string, string) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		if os.Getenv("OUTBOX_REQUIRE_PG") == "1" {
			t.Fatal("OUTBOX_PG_REQUIRED: missing disposable DATABASE_URL")
		}
		t.Skip("NOT_RUN: disposable migrated PG18 DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var tenantID, eventID string
	if err := admin.QueryRow(ctx, "INSERT INTO app.tenants(name) VALUES('c04b-outbox-test') RETURNING id::text").Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanupCtx, "DELETE FROM app.outbox_events WHERE tenant_id=$1::text::uuid", tenantID); err != nil {
			t.Errorf("fixture cleanup events: %v", err)
		}
		if _, err := admin.Exec(cleanupCtx, "DELETE FROM app.tenants WHERE id=$1::text::uuid", tenantID); err != nil {
			t.Errorf("fixture cleanup tenant: %v", err)
		}
	})
	if err := admin.QueryRow(ctx, "INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES($1::text::uuid,'test.event','{\"body_ref\":\"test-only\"}','test:consumer','stable-key') RETURNING id::text", tenantID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 2
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		if _, err := c.Exec(ctx, "SET ROLE app_outbox_worker"); err != nil {
			return err
		}
		var role string
		if err := c.QueryRow(ctx, "SELECT current_user::text").Scan(&role); err != nil {
			return err
		}
		if role != "app_outbox_worker" {
			return errors.New("OUTBOX_WORKER_ROLE_REQUIRED")
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx, admin, outbox.PGStore{Pool: pool}, tenantID, eventID
}

func outboxPGDispatcher(s outbox.Store, send outboxSenderFunc) outbox.Dispatcher {
	return outbox.Dispatcher{Store: s, Sender: send, Config: outbox.Config{WorkerID: outboxWorker, LeaseSeconds: 60, BaseRetrySeconds: 2, MaxRetrySeconds: 8}}
}

func TestPGDispatchCommittedClaimAndWorkerScope(t *testing.T) {
	ctx, admin, store, tenantID, eventID := outboxPGFixture(t)
	sent := 0
	d := outboxPGDispatcher(store, func(sendCtx context.Context, e outbox.Delivery) error {
		sent++
		var status, owner string
		var attempt int32
		var until time.Time
		if err := admin.QueryRow(sendCtx, "SELECT status,lease_owner::text,attempts,lease_until FROM app.outbox_events WHERE tenant_id=$1::text::uuid AND id=$2::text::uuid", tenantID, eventID).Scan(&status, &owner, &attempt, &until); err != nil {
			t.Fatal(err)
		}
		if status != "processing" || attempt != 1 || owner != outboxWorker {
			t.Fatalf("claim not committed before sender: %s %d %s", status, attempt, owner)
		}
		deadline, ok := sendCtx.Deadline()
		if !ok || deadline.After(until) {
			t.Fatalf("network deadline outlives lease: %v %v", deadline, until)
		}
		if e.TenantID != tenantID || e.EventID != eventID || e.Target != "test:consumer" || e.DedupKey != "stable-key" {
			t.Fatalf("delivery identity drift: %+v", e)
		}
		return nil
	})
	r, err := d.Dispatch(ctx, tenantID, 1)
	if err != nil || r != (outbox.Result{Claimed: 1, Sent: 1, Completed: 1}) || sent != 1 {
		t.Fatalf("dispatch %+v %v", r, err)
	}
	var status string
	var delivered, scopeCleared bool
	if err := admin.QueryRow(ctx, "SELECT status,delivered_at IS NOT NULL FROM app.outbox_events WHERE id=$1::text::uuid", eventID).Scan(&status, &delivered); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" || !delivered {
		t.Fatalf("ack not committed: %s %v", status, delivered)
	}
	if err := store.Pool.QueryRow(ctx, "SELECT nullif(current_setting('app.tenant_id',true),'') IS NULL").Scan(&scopeCleared); err != nil || !scopeCleared {
		t.Fatalf("tenant GUC escaped transaction: %v %v", scopeCleared, err)
	}
}

func TestPGSendFailurePersistsExponentialRetry(t *testing.T) {
	ctx, admin, store, tenantID, eventID := outboxPGFixture(t)
	d := outboxPGDispatcher(store, func(context.Context, outbox.Delivery) error {
		return errors.New("secret body and credentials must not persist")
	})
	for attempt, delay := range []int{2, 4, 8, 8} {
		var before, after, next time.Time
		if err := admin.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&before); err != nil {
			t.Fatal(err)
		}
		r, err := d.Dispatch(ctx, tenantID, 1)
		if err != nil || r != (outbox.Result{Claimed: 1, Failed: 1}) {
			t.Fatalf("retry %+v %v", r, err)
		}
		var status, code string
		var attempts int
		if err := admin.QueryRow(ctx, "SELECT status,last_error,attempts,next_attempt_at,clock_timestamp() FROM app.outbox_events WHERE id=$1::text::uuid", eventID).Scan(&status, &code, &attempts, &next, &after); err != nil {
			t.Fatal(err)
		}
		if status != "failed" || code != "OUTBOX_SEND_FAILED" || attempts != attempt+1 || next.Before(before.Add(time.Duration(delay)*time.Second)) || next.After(after.Add(time.Duration(delay)*time.Second)) {
			t.Fatalf("incorrect retry: %s %s %d %v", status, code, attempts, next)
		}
		leases, err := store.Claim(ctx, tenantID, outboxWorker, 1, 60)
		if err != nil || len(leases) != 0 {
			t.Fatalf("retry claimed before due %v %v", leases, err)
		}
		if _, err := admin.Exec(ctx, "UPDATE app.outbox_events SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1::text::uuid", eventID); err != nil {
			t.Fatal(err)
		}
	}
}

type outboxAckCrashStore struct {
	outbox.Store
	lost outbox.Lease
}

func (s *outboxAckCrashStore) Complete(_ context.Context, l outbox.Lease) (bool, error) {
	s.lost = l
	return false, errors.New("OUTBOX_TEST_ACK_CRASH")
}

func TestPGAckCrashTakeoverDedupAndPreviousAttemptFence(t *testing.T) {
	ctx, admin, store, tenantID, eventID := outboxPGFixture(t)
	consumer, err := admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Release()
	if _, err := consumer.Exec(ctx, "CREATE TEMP TABLE c04b_consumer_receipts(tenant_id text,event_id text,target text,PRIMARY KEY(tenant_id,event_id,target))"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := consumer.Exec(cleanupCtx, "DROP TABLE c04b_consumer_receipts"); err != nil {
			t.Errorf("consumer fixture cleanup: %v", err)
		}
	}()
	crash := &outboxAckCrashStore{Store: store}
	sends := 0
	sender := func(sendCtx context.Context, e outbox.Delivery) error {
		sends++
		if e.EventID != eventID || e.Target != "test:consumer" || e.DedupKey != "stable-key" {
			t.Fatal("unstable consumer dedup identity")
		}
		if sends == 2 {
			// Same worker UUID is reused; current lease is live, only old attempt differs.
			if ok, err := store.Complete(sendCtx, crash.lost); err != nil || ok {
				t.Fatalf("previous attempt ack accepted: %v %v", ok, err)
			}
			if ok, err := store.Fail(sendCtx, crash.lost, "OUTBOX_SEND_FAILED", 2); err != nil || ok {
				t.Fatalf("previous attempt fail accepted: %v %v", ok, err)
			}
		}
		_, err := consumer.Exec(sendCtx, "INSERT INTO c04b_consumer_receipts(tenant_id,event_id,target) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", e.TenantID, e.EventID, e.Target)
		return err
	}
	r, err := outboxPGDispatcher(crash, sender).Dispatch(ctx, tenantID, 1)
	if err == nil || err.Error() != "OUTBOX_TEST_ACK_CRASH" || r.Sent != 1 || r.Completed != 0 {
		t.Fatalf("crash evidence %+v %v", r, err)
	}
	if _, err := admin.Exec(ctx, "UPDATE app.outbox_events SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1::text::uuid", eventID); err != nil {
		t.Fatal(err)
	}
	r, err = outboxPGDispatcher(store, sender).Dispatch(ctx, tenantID, 1)
	if err != nil || r.Completed != 1 || sends != 2 {
		t.Fatalf("takeover %+v %v sends=%d", r, err, sends)
	}
	var receipts, attempts int
	var status string
	if err := consumer.QueryRow(ctx, "SELECT count(*) FROM c04b_consumer_receipts").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "SELECT attempts,status FROM app.outbox_events WHERE id=$1::text::uuid", eventID).Scan(&attempts, &status); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || attempts != 2 || status != "delivered" {
		t.Fatalf("at-least-once/dedup evidence %d %d %s", receipts, attempts, status)
	}
}

func TestPGWorkerCannotWriteTablesOrClaimWithoutScope(t *testing.T) {
	ctx, _, store, tenantID, _ := outboxPGFixture(t)
	_, err := store.Pool.Exec(ctx, "UPDATE app.outbox_events SET attempts=0 WHERE tenant_id=$1::text::uuid", tenantID)
	var pgErr *pgxadapter.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("worker direct write not denied: %v", err)
	}
	_, err = store.Pool.Exec(ctx, "SELECT app.claim_outbox($1::text::uuid,$2::text::uuid,1,60)", tenantID, outboxWorker)
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("unscoped worker claim not denied: %v", err)
	}
}

func TestPGClaimSkipsLockedAndExpiredSendCannotAck(t *testing.T) {
	ctx, admin, store, tenantID, eventID := outboxPGFixture(t)
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "SELECT id FROM app.outbox_events WHERE id=$1::text::uuid FOR UPDATE", eventID); err != nil {
		t.Fatal(err)
	}
	leases, err := store.Claim(ctx, tenantID, outboxWorker, 1, 60)
	if err != nil || len(leases) != 0 {
		t.Fatalf("locked claim not skipped %v %v", leases, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	d := outboxPGDispatcher(store, func(context.Context, outbox.Delivery) error {
		_, err := admin.Exec(ctx, "UPDATE app.outbox_events SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1::text::uuid", eventID)
		return err
	})
	r, err := d.Dispatch(ctx, tenantID, 1)
	if !errors.Is(err, outbox.ErrLeaseLost) || r.Completed != 0 || r.Sent != 1 {
		t.Fatalf("expired send confirmed %+v %v", r, err)
	}
}

const outboxWorker = "22222222-2222-4222-8222-222222222222"

type outboxSenderFunc func(context.Context, outbox.Delivery) error

func (f outboxSenderFunc) Send(ctx context.Context, d outbox.Delivery) error { return f(ctx, d) }
