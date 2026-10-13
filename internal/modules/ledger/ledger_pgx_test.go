//go:build pgx

// Responsibility: exercise the public ledger port against migrated PostgreSQL.
// Dependencies: pgxadapter (the only pool owner), SQLC and DATABASE_URL for an
// expendable database; module gate forbids raw pgx connections here.
// Invariants: each fixture uses a new tenant; authority is transaction-local;
// committed concurrency fixtures remain only in the expendable gate database.
package ledger_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"os"
	"sync"
	"testing"
	"time"
)

var errRollback = errors.New("fixture rollback")

func ledgerDB(t *testing.T) (context.Context, pgxadapter.Runner) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: expendable migrated PostgreSQL DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	r, closePool, err := pgxadapter.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closePool)
	return ctx, r
}

// inRolledBack runs fn in one maintenance transaction that is always rolled back.
func inRolledBack(t *testing.T, ctx context.Context, r pgxadapter.Runner, fn func(pgx.Tx)) {
	t.Helper()
	err := r.InMaintenance(ctx, func(_ context.Context, tx pgx.Tx) error { fn(tx); return errRollback })
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
}
func seedLedger(t *testing.T, ctx context.Context, tx pgx.Tx) (string, []string) {
	t.Helper()
	var tenant string
	if err := tx.QueryRow(ctx, "INSERT INTO app.tenants(name,settlement_currency) VALUES('ledger-go-gate','USD') RETURNING id::text").Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	accounts := make([]string, 5)
	for i, kind := range []string{"asset", "expense", "liability", "equity", "revenue"} {
		if err := tx.QueryRow(ctx, "INSERT INTO app.ledger_accounts(tenant_id,code,currency,account_type) VALUES($1,$2,'USD',$2) RETURNING id::text", tenant, kind).Scan(&accounts[i]); err != nil {
			t.Fatal(err)
		}
	}
	return tenant, accounts
}
func scopeLedger(t *testing.T, ctx context.Context, tx pgx.Tx, tenant string) {
	t.Helper()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatal(err)
	}
}
func journal(tenant string, accounts []string, event string) contract.Transaction {
	x := input()
	x.TenantID = tenant
	x.Event = event
	x.Entries[0].AccountID = accounts[0]
	x.Entries[1].AccountID = accounts[4]
	return x
}
func testUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan(s); err != nil {
		t.Fatal(err)
	}
	return id
}
func TestLedgerPostBalanceReplayReversal(t *testing.T) {
	ctx, r := ledgerDB(t)
	inRolledBack(t, ctx, r, func(tx pgx.Tx) { postBalanceReplayReversal(t, ctx, tx) })
}
func postBalanceReplayReversal(t *testing.T, ctx context.Context, tx pgx.Tx) {
	tenant, accounts := seedLedger(t, ctx, tx)
	foreignTenant, foreign := seedLedger(t, ctx, tx)
	scopeLedger(t, ctx, tx, tenant)
	m := ledger.New()
	x := journal(tenant, accounts, "captured")
	id, err := m.Post(ctx, tx, x)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := m.Post(ctx, tx, x)
	if err != nil || replay != id {
		t.Fatalf("same content: %s != %s; %v", replay, id, err)
	}
	stored, err := db.New(tx).GetLedgerTransaction(ctx, db.GetLedgerTransactionParams{TenantID: testUUID(t, tenant), TransactionID: testUUID(t, id)})
	if err != nil || stored.Status != "posted" || stored.Event != "captured" || !stored.PostedAt.Valid {
		t.Fatalf("posted row: %+v %v", stored, err)
	}
	before := stored.PostedAt.Time.Add(-time.Microsecond)
	b, err := m.Balance(ctx, tx, tenant, accounts[0], before)
	if err != nil || b.NormalNet.Amount != 0 {
		t.Fatalf("cutoff-before: %+v %v", b, err)
	}
	at := stored.PostedAt.Time
	for _, account := range []string{accounts[0], accounts[4]} {
		b, err = m.Balance(ctx, tx, tenant, account, at)
		if err != nil || b.NormalNet.Amount != 100 {
			t.Fatalf("cutoff-inclusive: %+v %v", b, err)
		}
	}
	// A caller-owned savepoint allows checking rejected commands without retaining
	// PostgreSQL's aborted transaction state; the module owns no savepoint itself.
	reject := func(name string, alter func(*contract.Transaction), want error) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			sp, err := tx.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer sp.Rollback(ctx)
			changed := journal(tenant, accounts, "captured")
			alter(&changed)
			_, err = m.Post(ctx, sp, changed)
			if !errors.Is(err, want) {
				t.Fatalf("got %v; want %v", err, want)
			}
		})
	}
	reject("changed amount", func(x *contract.Transaction) { x.Entries[0].Amount.Amount = 101; x.Entries[1].Amount.Amount = 101 }, contract.ErrConflict)
	reject("changed order", func(x *contract.Transaction) { x.Entries[0], x.Entries[1] = x.Entries[1], x.Entries[0] }, contract.ErrConflict)
	reject("foreign account", func(x *contract.Transaction) { x.Event = "foreign-account"; x.Entries[0].AccountID = foreign[0] }, contract.ErrNotFound)
	reject("foreign tenant", func(x *contract.Transaction) {
		x.TenantID = foreignTenant
		x.Entries[0].AccountID = foreign[0]
		x.Entries[1].AccountID = foreign[4]
	}, contract.ErrForbidden)
	missing := "00000000-0000-0000-0000-ffffffffffff"
	if _, err := m.Balance(ctx, tx, tenant, missing, time.Now()); !errors.Is(err, contract.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := m.Balance(ctx, tx, foreignTenant, foreign[0], time.Now()); !errors.Is(err, contract.ErrNotFound) {
		t.Fatalf("foreign balance leaked: %v", err)
	}
	reversal := journal(tenant, accounts, "reversed")
	reversal.Entries[0].Side = contract.Credit
	reversal.Entries[1].Side = contract.Debit
	reverseID, err := m.Post(ctx, tx, reversal)
	if err != nil || reverseID == id {
		t.Fatalf("new reversal identity: %s %v", reverseID, err)
	}
	for _, account := range []string{accounts[0], accounts[4]} {
		b, err = m.Balance(ctx, tx, tenant, account, time.Now())
		if err != nil || b.NormalNet.Amount != 0 || b.Debits.Amount != 100 || b.Credits.Amount != 100 {
			t.Fatalf("reversal balance: %+v %v", b, err)
		}
	}
	// Force deferred constraints now: rollback cleanup alone cannot prove finality.
	if _, err := tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
}
func TestLedgerAllNormalBalances(t *testing.T) {
	ctx, r := ledgerDB(t)
	inRolledBack(t, ctx, r, func(tx pgx.Tx) { allNormalBalances(t, ctx, tx) })
}
func allNormalBalances(t *testing.T, ctx context.Context, tx pgx.Tx) {
	var err error
	tenant, accounts := seedLedger(t, ctx, tx)
	scopeLedger(t, ctx, tx, tenant)
	m := ledger.New()
	for i, account := range accounts {
		entries := []contract.Entry{{AccountID: account, Side: contract.Debit, Amount: money.Money{Amount: 17, Currency: "USD"}}, {AccountID: accounts[(i+1)%len(accounts)], Side: contract.Credit, Amount: money.Money{Amount: 17, Currency: "USD"}}}
		x := journal(tenant, accounts, string(rune('a'+i)))
		x.Entries = entries
		if _, err = m.Post(ctx, tx, x); err != nil {
			t.Fatal(err)
		}
	}
	// Equal ring totals, followed by debits to each remaining account and an
	// asset counter-credit, make every account's normal side observable.
	for i := 1; i < len(accounts); i++ {
		x := journal(tenant, accounts, string(rune('k'+i)))
		x.Entries[0].AccountID = accounts[i]
		x.Entries[1].AccountID = accounts[0]
		if _, err = m.Post(ctx, tx, x); err != nil {
			t.Fatal(err)
		}
	}
	for i, account := range accounts {
		b, err := m.Balance(ctx, tx, tenant, account, time.Now())
		want := int64(-100)
		if i == 0 {
			want = -400
		}
		if i == 1 {
			want = 100
		}
		if err != nil || b.NormalNet.Amount != want {
			t.Fatalf("account %d: %+v %v; want %d", i, b, err, want)
		}
	}
	if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
}
func TestLedgerConcurrentIdenticalReplay(t *testing.T) {
	ctx, r := ledgerDB(t)
	var tenant string
	var accounts []string
	if err := r.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tenant, accounts = seedLedger(t, ctx, tx)
		return nil // committed: the racing transactions must see the fixture
	}); err != nil {
		t.Fatal(err)
	}
	type result struct {
		id  string
		err error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			// Each InTenant call takes its own pooled connection; both
			// transactions are open and scoped before either posts.
			var once sync.Once
			signal := func() { once.Do(ready.Done) }
			var id string
			err := r.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime")
				signal()
				<-start
				if err != nil {
					return err
				}
				id, err = ledger.New().Post(ctx, tx, journal(tenant, accounts, "concurrent"))
				return err
			})
			signal()
			results <- result{id: id, err: err}
		}()
	}
	ready.Wait()
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.id == "" || first.id != second.id {
		t.Fatalf("concurrent replay: %+v %+v", first, second)
	}
	inRolledBack(t, ctx, r, func(tx pgx.Tx) {
		scopeLedger(t, ctx, tx, tenant)
		b, err := ledger.New().Balance(ctx, tx, tenant, accounts[0], time.Now())
		if err != nil || b.Debits.Amount != 100 || b.Credits.Amount != 0 {
			t.Fatalf("duplicate committed content: %+v %v", b, err)
		}
	})
}
