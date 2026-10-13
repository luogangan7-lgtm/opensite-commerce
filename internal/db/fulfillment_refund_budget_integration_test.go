//go:build pgx

// Responsibilities: prove shared refund budget locking on real PostgreSQL.
// Dependencies: existing mandatory fulfillment fixtures, pgxadapter transactions
// and public Orders/Returns SQL commands; called by the tagged race CI gate.
// Invariants: parent then UUID-sorted RMA locks, stale RR writes retry, and
// independent legacy/RMA arithmetic conserves paid money and restores once.
package db_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type budgetSession struct {
	pid  int32
	done <-chan error
	stop context.CancelFunc
}

// All participants use the authorized adapter. Channels park a transaction;
// pg_blocking_pids, rather than elapsed time, proves the command reached a lock.
func startBudgetSession(t *testing.T, f fulfillmentFixture, role string, repeatable bool, fn func(context.Context, pgx.Tx) error) budgetSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
	t.Cleanup(cancel)
	pids, done := make(chan int32, 1), make(chan error, 1)
	go func() {
		done <- f.maint.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if repeatable {
				if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", f.tenant); err != nil {
				return err
			}
			statement := "SET LOCAL ROLE app_runtime"
			if role == "service" {
				statement = "SET LOCAL ROLE app_returns_service"
			} else if role == "maintenance" {
				statement = "RESET ROLE"
			}
			if _, err := tx.Exec(ctx, statement); err != nil {
				return err
			}
			var pid int32
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			pids <- pid
			return fn(ctx, tx)
		})
	}()
	select {
	case pid := <-pids:
		return budgetSession{pid: pid, done: done, stop: cancel}
	case err := <-done:
		// A NOWAIT probe can finish before the caller consumes its buffered PID.
		select {
		case pid := <-pids:
			done <- err
			return budgetSession{pid: pid, done: done, stop: cancel}
		default:
		}
		cancel()
		t.Fatalf("budget participant failed before PID: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return budgetSession{}
}

func awaitBudgetBlock(t *testing.T, f fulfillmentFixture, waiter, blocker int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		var blocked bool
		err := f.maint.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT $2::integer=ANY(pg_blocking_pids($1::integer))", waiter, blocker).Scan(&blocked)
		})
		if err != nil {
			t.Fatalf("budget lock witness: %v", err)
		}
		if blocked {
			return
		}
	}
	t.Fatalf("budget lock order missing: PID %d never blocked on %d", waiter, blocker)
}

func budgetDone(t *testing.T, f fulfillmentFixture, s budgetSession) error {
	t.Helper()
	select {
	case err := <-s.done:
		return err
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
		return f.ctx.Err()
	}
}

func budgetBarrier(t *testing.T, f fulfillmentFixture, ready <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatalf("budget participant did not reach transaction barrier: %v", ctx.Err())
	}
}

func budgetRelease() (chan struct{}, func()) {
	release := make(chan struct{})
	var once sync.Once
	return release, func() { once.Do(func() { close(release) }) }
}

func budgetPark(ctx context.Context, release <-chan struct{}) error {
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func budgetLegacy(ctx context.Context, tx pgx.Tx, order, key string, amount int64) (int64, error) {
	var version int64
	err := tx.QueryRow(ctx, "SELECT (app.orders_transition($1,'partially_refunded',$2,$3)).version", order, key, amount).Scan(&version)
	return version, err
}

func budgetRequest(ctx context.Context, tx pgx.Tx, f fulfillmentFixture, version, quantity int64, key string) (string, int64, error) {
	lines := fmt.Sprintf(`[{"order_line_id":%q,"quantity":%d}]`, f.line, quantity)
	var id string
	var amount int64
	err := tx.QueryRow(ctx, `WITH r AS MATERIALIZED (SELECT app.returns_create($1,$2,$3,'unshipped_cancel','budget race',$4::jsonb) AS value)
	 SELECT value->>'id',(value->>'refund_minor')::bigint FROM r`, f.order.ID, version, key, lines).Scan(&id, &amount)
	return id, amount, err
}

func newBudgetReturn(t *testing.T, f fulfillmentFixture, quantity int64, key string) (string, int64) {
	t.Helper()
	version, _, _ := conservationOrder(t, f)
	var id string
	var amount int64
	err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, amount, err = budgetRequest(ctx, tx, f, version, quantity, key)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id, amount
}

// A valid posted public Ledger receipt isolates the database completion gate;
// provider orchestration remains covered by the existing conservation tests.
func prepareBudgetReturn(t *testing.T, f fulfillmentFixture, id, key string, amount int64) string {
	t.Helper()
	version, _, _ := conservationOrder(t, f)
	var income, tax, shipping, cash string
	// The service may invoke the command but cannot read account configuration.
	if err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='sales'),
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='tax'),
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='shipping'),
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='cash')`, f.tenant).Scan(&income, &tax, &shipping, &cash)
	}); err != nil {
		t.Fatal(err)
	}
	var journal string
	err := f.service.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT app.returns_prepare_refund($1,$2,$3,'budget-provider',1,$4,$5,$6,$7)", id, version, key, income, tax, shipping, cash); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT app.post_ledger($1,'USD','return',$2,'refund',
		 ARRAY[$4,$5]::uuid[],ARRAY['debit','credit'],ARRAY[$3,$3]::bigint[])::text`, f.tenant, id, amount, income, cash).Scan(&journal)
	})
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

func completeBudgetReturn(ctx context.Context, tx pgx.Tx, id, key, journal string) error {
	_, err := tx.Exec(ctx, "SELECT app.returns_complete_refund($1,$2,'budget-provider',$3)", id, key, journal)
	return err
}

func finishBudgetReturn(t *testing.T, f fulfillmentFixture, id, key string, amount int64) {
	t.Helper()
	journal := prepareBudgetReturn(t, f, id, key, amount)
	if err := f.service.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return completeBudgetReturn(ctx, tx, id, key, journal)
	}); err != nil {
		t.Fatal(err)
	}
}

func assertBudgetFinal(t *testing.T, f fulfillmentFixture, initialStock, legacy, rmas int64) {
	t.Helper()
	_, refunded, state := conservationOrder(t, f)
	var frozen, moves, journals, credits, unbalanced, events int64
	err := f.maint.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
		 (SELECT sum(refund_minor) FROM app.returns WHERE tenant_id=$1 AND order_id=$2 AND status='refunded'),
		 (SELECT count(*) FROM app.inventory_movements WHERE tenant_id=$1 AND owner_type='return'),
		 (SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=$1 AND source_type='return' AND event='refund'),
		 (SELECT sum(amount) FROM app.ledger_entries WHERE tenant_id=$1 AND side='credit'),
		 (SELECT count(*) FROM (SELECT transaction_id FROM app.ledger_entries WHERE tenant_id=$1 GROUP BY transaction_id HAVING sum(CASE WHEN side='debit' THEN amount ELSE -amount END)<>0) x),
		 (SELECT count(*) FROM app.outbox_events WHERE tenant_id=$1 AND payload->>'order_id'=$2::text AND event_type IN('order.partially_refunded','order.refunded'))`, f.tenant, f.order.ID).Scan(&frozen, &moves, &journals, &credits, &unbalanced, &events)
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyEvents := int64(0)
	if legacy > 0 {
		legacyEvents = 1
	}
	if refunded != 14 || state != "refunded" || frozen+legacy != 14 || moves != rmas || journals != rmas || credits != 14-legacy || unbalanced != 0 || events != rmas+legacyEvents || f.stock(t) != initialStock {
		t.Fatalf("budget conservation: order=%d state=%s frozen=%d legacy=%d moves=%d journals=%d credits=%d unbalanced=%d events=%d stock=%d want=%d", refunded, state, frozen, legacy, moves, journals, credits, unbalanced, events, f.stock(t), initialStock)
	}
}

func TestRefundBudgetConcurrentLegacyThenFreeze(t *testing.T) {
	f, stock := newConservationFixture(t, 2, 7, 0, 0, 0)
	release, unblock := budgetRelease()
	defer unblock()
	ready := make(chan struct{})
	var version int64
	first := startBudgetSession(t, f, "runtime", false, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		version, err = budgetLegacy(ctx, tx, f.order.ID, "budget-legacy-first", 2)
		if err != nil {
			return err
		}
		close(ready)
		return budgetPark(ctx, release)
	})
	budgetBarrier(t, f, ready)
	var id string
	var amount int64
	second := startBudgetSession(t, f, "runtime", false, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, amount, err = budgetRequest(ctx, tx, f, version, 2, "budget-after-legacy")
		return err
	})
	awaitBudgetBlock(t, f, second.pid, first.pid)
	unblock()
	if err := budgetDone(t, f, first); err != nil {
		t.Fatal(err)
	}
	if err := budgetDone(t, f, second); err != nil || amount != 12 {
		t.Fatalf("legacy2 leaves frozen12: amount=%d err=%v", amount, err)
	}
	finishBudgetReturn(t, f, id, "budget-after-legacy", 12)
	assertBudgetFinal(t, f, stock, 2, 1)
}

func TestRefundBudgetConcurrentFreezeFencesLegacy(t *testing.T) {
	f, stock := newConservationFixture(t, 2, 7, 0, 0, 0)
	version, _, _ := conservationOrder(t, f)
	release, unblock := budgetRelease()
	defer unblock()
	ready := make(chan struct{})
	var id string
	var amount int64
	first := startBudgetSession(t, f, "runtime", false, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, amount, err = budgetRequest(ctx, tx, f, version, 1, "budget-frozen-first")
		if err != nil {
			return err
		}
		close(ready)
		return budgetPark(ctx, release)
	})
	budgetBarrier(t, f, ready)
	second := startBudgetSession(t, f, "runtime", false, func(ctx context.Context, tx pgx.Tx) error {
		_, err := budgetLegacy(ctx, tx, f.order.ID, "budget-fenced", 2)
		return err
	})
	awaitBudgetBlock(t, f, second.pid, first.pid)
	unblock()
	if err := budgetDone(t, f, first); err != nil || amount != 7 {
		t.Fatalf("first freeze: amount=%d err=%v", amount, err)
	}
	var pgerr *pgconn.PgError
	if err := budgetDone(t, f, second); !errors.As(err, &pgerr) || pgerr.Code != "55000" || pgerr.Message != "FULFILLMENT_PENDING_REFUND_COMMAND_REQUIRED" {
		t.Fatalf("legacy ignored frozen RMA: %v", err)
	}
	finishBudgetReturn(t, f, id, "budget-frozen-first", 7)
	if err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := budgetLegacy(ctx, tx, f.order.ID, "budget-after-terminal", 2)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	id, amount = newBudgetReturn(t, f, 1, "budget-last-five")
	if amount != 5 {
		t.Fatalf("7 RMA +2 legacy leaves5, got%d", amount)
	}
	finishBudgetReturn(t, f, id, "budget-last-five", 5)
	assertBudgetFinal(t, f, stock, 2, 2)
}

func TestRefundBudgetLocksOrderThenAllReturnsByUUID(t *testing.T) {
	f, stock := newConservationFixture(t, 2, 7, 0, 0, 0)
	a, aa := newBudgetReturn(t, f, 1, "budget-sort-a")
	b, ba := newBudgetReturn(t, f, 1, "budget-sort-b")
	if aa != 7 || ba != 7 {
		t.Fatal("independent halves must each freeze7")
	}
	ids := []string{a, b}
	sort.Strings(ids)
	keys := map[string]string{a: "budget-sort-a", b: "budget-sort-b"}
	low, high := ids[0], ids[1]
	jlow := prepareBudgetReturn(t, f, low, keys[low], 7)
	jhigh := prepareBudgetReturn(t, f, high, keys[high], 7)
	// Holding the parent must block completion before it acquires any child.
	parentRelease, freeParent := budgetRelease()
	defer freeParent()
	parentReady := make(chan struct{})
	parent := startBudgetSession(t, f, "maintenance", false, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT id FROM app.orders WHERE tenant_id=$1 AND id=$2 FOR UPDATE", f.tenant, f.order.ID); err != nil {
			return err
		}
		close(parentReady)
		return budgetPark(ctx, parentRelease)
	})
	budgetBarrier(t, f, parentReady)
	parentWaiter := startBudgetSession(t, f, "service", false, func(ctx context.Context, tx pgx.Tx) error {
		return completeBudgetReturn(ctx, tx, low, keys[low], jlow)
	})
	awaitBudgetBlock(t, f, parentWaiter.pid, parent.pid)
	childProbe := startBudgetSession(t, f, "maintenance", false, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT id FROM app.returns WHERE tenant_id=$1 AND id=$2 FOR UPDATE NOWAIT", f.tenant, low)
		return err
	})
	if err := budgetDone(t, f, childProbe); err != nil {
		t.Fatalf("completion locked child before blocked parent: %v", err)
	}
	parentWaiter.stop()
	if err := budgetDone(t, f, parentWaiter); err == nil {
		t.Fatal("cancelled order-first witness unexpectedly committed")
	}
	freeParent()
	if err := budgetDone(t, f, parent); err != nil {
		t.Fatal(err)
	}
	release, unblock := budgetRelease()
	defer unblock()
	ready := make(chan struct{})
	holder := startBudgetSession(t, f, "maintenance", false, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT id FROM app.returns WHERE tenant_id=$1 AND id=$2 FOR UPDATE", f.tenant, high); err != nil {
			return err
		}
		close(ready)
		return budgetPark(ctx, release)
	})
	budgetBarrier(t, f, ready)
	completion := startBudgetSession(t, f, "service", false, func(ctx context.Context, tx pgx.Tx) error {
		return completeBudgetReturn(ctx, tx, low, keys[low], jlow)
	})
	awaitBudgetBlock(t, f, completion.pid, holder.pid)
	legacy := startBudgetSession(t, f, "runtime", false, func(ctx context.Context, tx pgx.Tx) error {
		_, err := budgetLegacy(ctx, tx, f.order.ID, "budget-parent-probe", 1)
		return err
	})
	awaitBudgetBlock(t, f, legacy.pid, completion.pid)
	legacy.stop()
	if err := budgetDone(t, f, legacy); err == nil {
		t.Fatal("cancelled parent-lock witness unexpectedly committed")
	}
	// Reuse the freed slot: at most three participants plus one observer. The
	// lowest UUID must already be locked while completion waits on the highest.
	lower := startBudgetSession(t, f, "maintenance", false, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT id FROM app.returns WHERE tenant_id=$1 AND id=$2 FOR UPDATE", f.tenant, low)
		return err
	})
	awaitBudgetBlock(t, f, lower.pid, completion.pid)
	unblock()
	for _, s := range []budgetSession{holder, completion, lower} {
		if err := budgetDone(t, f, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return completeBudgetReturn(ctx, tx, high, keys[high], jhigh)
	}); err != nil {
		t.Fatal(err)
	}
	assertBudgetFinal(t, f, stock, 0, 2)
}

func TestRefundBudgetRepeatableReadLegacyRetriesAfterCompletion(t *testing.T) {
	f, stock := newConservationFixture(t, 2, 7, 0, 0, 0)
	id, amount := newBudgetReturn(t, f, 1, "budget-rr-first")
	if amount != 7 {
		t.Fatal("first half must freeze7")
	}
	journal := prepareBudgetReturn(t, f, id, "budget-rr-first", 7)
	release, unblock := budgetRelease()
	defer unblock()
	ready := make(chan struct{})
	stale := startBudgetSession(t, f, "runtime", true, func(ctx context.Context, tx pgx.Tx) error {
		var before int64
		if err := tx.QueryRow(ctx, "SELECT refunded_minor FROM app.orders WHERE tenant_id=$1 AND id=$2", f.tenant, f.order.ID).Scan(&before); err != nil {
			return err
		}
		if before != 0 {
			return fmt.Errorf("RR fixture expected confirmed0, got%d", before)
		}
		close(ready)
		if err := budgetPark(ctx, release); err != nil {
			return err
		}
		_, err := budgetLegacy(ctx, tx, f.order.ID, "budget-rr-legacy", 2)
		return err
	})
	budgetBarrier(t, f, ready)
	if err := f.service.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return completeBudgetReturn(ctx, tx, id, "budget-rr-first", journal)
	}); err != nil {
		t.Fatal(err)
	}
	unblock()
	var pgerr *pgconn.PgError
	if err := budgetDone(t, f, stale); !errors.As(err, &pgerr) || pgerr.Code != "40001" {
		t.Fatalf("stale RR budget write must serialize/retry: %v", err)
	}
	_, confirmed, _ := conservationOrder(t, f)
	if confirmed != 7 {
		t.Fatalf("stale transaction leaked credit: %d", confirmed)
	}
	if err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := budgetLegacy(ctx, tx, f.order.ID, "budget-rr-legacy", 2)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	id, amount = newBudgetReturn(t, f, 1, "budget-rr-last")
	if amount != 5 {
		t.Fatalf("retry7+2 leaves5, got%d", amount)
	}
	finishBudgetReturn(t, f, id, "budget-rr-last", 5)
	assertBudgetFinal(t, f, stock, 2, 2)
}
