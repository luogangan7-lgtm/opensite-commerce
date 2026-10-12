//go:build pgx

// Real PG18 tests exercise generated production queries and app_runtime RLS.
// DATABASE_URL must target the parent's disposable migrated database. Unique
// tenants are cleaned by the admin fixture connection; no arbitrary sleep is
// a scheduling witness: advisory locks plus pg_stat_activity prove overlap.
package idempotency

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

type pgFixture struct {
	ctx     context.Context
	cancel  context.CancelFunc
	admin   *pgx.Conn
	dsn     string
	tenants []string
	actor   string
}

func fixture(t *testing.T) *pgFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		if os.Getenv("IDEMPOTENCY_REQUIRE_PG") == "1" {
			t.Fatal("DATABASE_URL required by idempotency PG gate")
		}
		t.Skip("NOT_RUN: disposable migrated PostgreSQL DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	f := &pgFixture{ctx: ctx, cancel: cancel, admin: admin, dsn: dsn, actor: testID(t)}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		for _, tenant := range f.tenants {
			for _, query := range []string{"DELETE FROM app.idempotency_records WHERE tenant_id=$1", "DELETE FROM app.organizations WHERE tenant_id=$1", "DELETE FROM app.tenants WHERE id=$1"} {
				if _, err := f.admin.Exec(cleanup, query, parseUUID(tenant)); err != nil {
					t.Errorf("fixture cleanup:%v", err)
				}
			}
		}
		_ = admin.Close(cleanup)
		cancel()
	})
	for range 2 {
		tenant := testID(t)
		if _, err = admin.Exec(ctx, "INSERT INTO app.tenants(id,name) VALUES($1,'c04b-idempotency-test')", parseUUID(tenant)); err != nil {
			t.Fatal(err)
		}
		f.tenants = append(f.tenants, tenant)
	}
	return f
}
func testID(t *testing.T) string {
	t.Helper()
	id, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	return uuidText(id)
}
func (f *pgFixture) connect(t *testing.T) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeConnection(c) })
	return c
}
func (f *pgFixture) scope(key string) Scope {
	return Scope{f.tenants[0], "service", f.actor, "testCommand", key}
}
func scoped(ctx context.Context, c *pgx.Conn, tenant string) (pgx.Tx, error) {
	tx, err := c.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", tenant); err == nil {
		_, err = tx.Exec(ctx, "SET LOCAL ROLE app_runtime")
	}
	if err != nil {
		rollback(tx)
		return nil, err
	}
	return tx, nil
}
func (f *pgFixture) tx(t *testing.T, tenant string) pgx.Tx {
	t.Helper()
	tx, err := scoped(f.ctx, f.admin, tenant)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rollback(tx) })
	return tx
}
func requireProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	var e *apperrors.Error
	if !errors.As(err, &e) || e.Status != status || e.Code != code {
		t.Fatalf("want%d/%s;got%v", status, code, err)
	}
}
func addEffect(ctx context.Context, tx pgx.Tx, scope Scope) error {
	_, err := tx.Exec(ctx, "INSERT INTO app.organizations(tenant_id,display_name) VALUES($1,$2)", parseUUID(scope.TenantID), scope.Key)
	return err
}
func effectCount(t *testing.T, f *pgFixture, scope Scope) int {
	t.Helper()
	var n int
	if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM app.organizations WHERE tenant_id=$1 AND display_name=$2", parseUUID(scope.TenantID), scope.Key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func testResponse() Response {
	return Response{201, http.Header{"Content-Type": {"application/octet-stream"}, "Etag": {"\"idem-v1\""}, "X-Repeat": {"a", "b"}}, []byte{0, 255, 'o', 'k'}}
}
func localResponses(t *testing.T) ResponseStore {
	t.Helper()
	s, err := NewLocalResponseStore(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPGIdempotencyExactReplayAndScope(t *testing.T) {
	f := fixture(t)
	store := localResponses(t)
	scope := f.scope("replay")
	hash := Fingerprint("POST", "/test/{id}", map[string]string{"id": "one"}, nil, nil)
	want := testResponse()
	tx := f.tx(t, scope.TenantID)
	got, err := ExecuteTx(f.ctx, tx, scope, hash, time.Hour, store, func(ctx context.Context, tx pgx.Tx) (Response, error) { return want, addEffect(ctx, tx, scope) })
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("execute:%+v %v", got, err)
	}
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	tx = f.tx(t, scope.TenantID)
	got, err = ExecuteTx(f.ctx, tx, scope, hash, time.Hour, store, func(context.Context, pgx.Tx) (Response, error) {
		t.Fatal("replay executed domain command")
		return Response{}, nil
	})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("replay:%+v %v", got, err)
	}
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	tx = f.tx(t, scope.TenantID)
	different := Fingerprint("POST", "/test/{id}", map[string]string{"id": "two"}, nil, nil)
	_, err = ClaimTx(f.ctx, tx, scope, different, time.Hour)
	requireProblem(t, err, 422, "idempotency_key_reused")
	_ = tx.Rollback(f.ctx)
	if n := effectCount(t, f, scope); n != 1 {
		t.Fatalf("domainwrites%d want1", n)
	}
	// Each full primary-key axis is distinct, and the transaction must match it.
	for axis := 0; axis < 4; axis++ {
		other := scope
		switch axis {
		case 0:
			other.TenantID = f.tenants[1]
		case 1:
			other.ActorID = testID(t)
		case 2:
			other.ActorKind = "api_key"
		case 3:
			other.Operation = "otherCommand"
		}
		tx = f.tx(t, other.TenantID)
		result, err := ClaimTx(f.ctx, tx, other, different, time.Hour)
		if err != nil || result.Claim == nil {
			t.Fatalf("independent scope axis%d:%+v %v", axis, result, err)
		}
		_ = tx.Rollback(f.ctx)
	}
	tx = f.tx(t, scope.TenantID)
	other := scope
	other.TenantID = f.tenants[1]
	_, err = ClaimTx(f.ctx, tx, other, hash, time.Hour)
	requireProblem(t, err, 403, "tenant_scope_mismatch")
	_ = tx.Rollback(f.ctx)
	tx = f.tx(t, scope.TenantID)
	if _, err = ClaimTx(f.ctx, tx, scope, hash, 0); !errors.Is(err, ErrInvalidTTL) {
		t.Fatal("zero TTL accepted", err)
	}
	_ = tx.Rollback(f.ctx)
}

func TestPGIdempotencyInProgressRollbackAndStorageFailure(t *testing.T) {
	f := fixture(t)
	scope := f.scope("in-progress")
	hash := Fingerprint("POST", "/test", nil, nil, nil)
	store := localResponses(t)
	tx := f.tx(t, scope.TenantID)
	first, err := ClaimTx(f.ctx, tx, scope, hash, time.Hour)
	if err != nil || first.Claim == nil {
		t.Fatal("first claim", err)
	}
	_, err = ClaimTx(f.ctx, tx, scope, hash, time.Hour)
	requireProblem(t, err, 409, "idempotency_in_progress")
	other := hash
	other[0] ^= 255
	_, err = ClaimTx(f.ctx, tx, scope, other, time.Hour)
	requireProblem(t, err, 422, "idempotency_key_reused")
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	// A committed active claim also reports409 in a fresh transaction.
	tx = f.tx(t, scope.TenantID)
	_, err = ClaimTx(f.ctx, tx, scope, hash, time.Hour)
	requireProblem(t, err, 409, "idempotency_in_progress")
	_ = tx.Rollback(f.ctx)
	for _, name := range []string{"domain-error", "store-error", "canceled"} {
		scope = f.scope(name)
		tx = f.tx(t, scope.TenantID)
		sentinel := errors.New("fixture failure")
		selected := store
		if name == "store-error" {
			selected = failingResponses{sentinel}
		}
		commandContext := f.ctx
		cancel := func() {}
		if name == "canceled" {
			commandContext, cancel = context.WithCancel(f.ctx)
		}
		_, err = ExecuteTx(commandContext, tx, scope, hash, time.Hour, selected, func(ctx context.Context, tx pgx.Tx) (Response, error) {
			if err := addEffect(ctx, tx, scope); err != nil {
				return Response{}, err
			}
			if name == "domain-error" {
				return Response{}, sentinel
			}
			if name == "canceled" {
				cancel()
			}
			return testResponse(), nil
		})
		cancel()
		if err == nil {
			t.Fatalf("%s unexpectedly committed", name)
		}
		rollback(tx)
		if effectCount(t, f, scope) != 0 {
			t.Fatalf("%s left domain effects", name)
		}
		tx = f.tx(t, scope.TenantID)
		r, err := ClaimTx(f.ctx, tx, scope, hash, time.Hour)
		if err != nil || r.Claim == nil {
			t.Fatalf("%s rollback left request occupied:%v", name, err)
		}
		_ = tx.Rollback(f.ctx)
	}
}

type failingResponses struct{ err error }

func (s failingResponses) Put(context.Context, Response) (string, error) { return "", s.err }
func (s failingResponses) Get(context.Context, string) (Response, error) { return Response{}, s.err }

func TestPGIdempotencyExpiryAndCompletionFence(t *testing.T) {
	f := fixture(t)
	scope := f.scope("expired")
	hash := Fingerprint("POST", "/test", nil, nil, nil)
	token, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	// Seed an already-expired claim instead of relying on wall-clock sleeps.
	_, err = f.admin.Exec(f.ctx, `INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,claim_token)
 VALUES($1,$2,$3,$4,$5,$6,clock_timestamp()-interval '1 day',$7)`, parseUUID(scope.TenantID), scope.ActorKind, parseUUID(scope.ActorID), scope.Operation, scope.Key, hash[:], token)
	if err != nil {
		t.Fatal(err)
	}
	stale := &Claim{scope, token, hash}
	changed := hash
	changed[0] ^= 1
	tx := f.tx(t, scope.TenantID)
	_, err = ClaimTx(f.ctx, tx, scope, changed, time.Hour)
	requireProblem(t, err, 422, "idempotency_key_reused")
	_ = tx.Rollback(f.ctx)
	tx = f.tx(t, scope.TenantID)
	current, err := ClaimTx(f.ctx, tx, scope, hash, time.Hour)
	if err != nil || current.Claim == nil || current.Claim.token == token {
		t.Fatal("expired same-request not reclaimed", err)
	}
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	store := localResponses(t)
	tx = f.tx(t, scope.TenantID)
	if err = CompleteTx(f.ctx, tx, stale, testResponse(), store); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale owner completed:%v", err)
	}
	_ = tx.Rollback(f.ctx)
	// Independent database trigger assertion: current row + WRONG claim GUC must
	// raise55000. This fails if the SQL fence is removed despite the Go predicate.
	tx = f.tx(t, scope.TenantID)
	_, err = tx.Exec(f.ctx, "SELECT set_config('app.idempotency_claim',$1,true)", uuidText(token))
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(f.ctx, `UPDATE app.idempotency_records SET state='completed',response_status=201,response_body_reference='fixture:response',completed_at=clock_timestamp()
 WHERE tenant_id=$1 AND actor_kind=$2 AND actor_id=$3 AND operation=$4 AND idempotency_key=$5`, parseUUID(scope.TenantID), scope.ActorKind, parseUUID(scope.ActorID), scope.Operation, scope.Key)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "55000" {
		t.Fatalf("DBwrongfence want55000 got%v", err)
	}
	_ = tx.Rollback(f.ctx)
	tx = f.tx(t, scope.TenantID)
	if err = CompleteTx(f.ctx, tx, current.Claim, testResponse(), store); err != nil {
		t.Fatal("current completion", err)
	}
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	tx = f.tx(t, scope.TenantID)
	replayed, err := ClaimTx(f.ctx, tx, scope, hash, time.Hour)
	if err != nil || replayed.Claim != nil || replayed.ReplayStatus != 201 {
		t.Fatal("takeover response not replayable", err)
	}
	_ = tx.Rollback(f.ctx)
}

func TestPGIdempotencyConcurrentClaims(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("different_hash_%t", changed), func(t *testing.T) {
			f := fixture(t)
			scope := f.scope("concurrent")
			hash := Fingerprint("POST", "/test", nil, nil, nil)
			secondHash := hash
			if changed {
				secondHash[0] ^= 255
			}
			store := localResponses(t)
			owner := f.connect(t)
			competitor := f.connect(t)
			gate := int64(time.Now().UnixNano()) // lock id only; time is NOT a schedule witness.
			if _, err := f.admin.Exec(f.ctx, "SELECT pg_advisory_lock($1)", gate); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			defer func() {
				// Unlock/cancel before waiting; every background query is bounded by ctx.
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				_, _ = f.admin.Exec(cleanup, "SELECT pg_advisory_unlock($1)", gate)
				stop()
				f.cancel()
				wg.Wait()
			}()
			ownerResult := make(chan error, 1)
			ready := make(chan struct{})
			wg.Add(1)
			go func() {
				defer wg.Done()
				tx, err := scoped(f.ctx, owner, scope.TenantID)
				if err != nil {
					ownerResult <- err
					return
				}
				defer rollback(tx)
				_, err = ExecuteTx(f.ctx, tx, scope, hash, time.Hour, store, func(ctx context.Context, tx pgx.Tx) (Response, error) {
					if err := addEffect(ctx, tx, scope); err != nil {
						return Response{}, err
					}
					close(ready)
					_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", gate)
					return testResponse(), err
				})
				if err == nil {
					err = tx.Commit(f.ctx)
				}
				ownerResult <- err
			}()
			select {
			case <-ready:
			case err := <-ownerResult:
				t.Fatal("owner failed before barrier", err)
			case <-f.ctx.Done():
				t.Fatal("owner barrier timeout")
			}
			awaitBlocking(t, f, owner.PgConn().PID(), f.admin.PgConn().PID())
			type answer struct {
				response Response
				err      error
			}
			competitorResult := make(chan answer, 1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				tx, err := scoped(f.ctx, competitor, scope.TenantID)
				if err != nil {
					competitorResult <- answer{err: err}
					return
				}
				defer rollback(tx)
				response, err := ExecuteTx(f.ctx, tx, scope, secondHash, time.Hour, store, func(ctx context.Context, tx pgx.Tx) (Response, error) {
					return testResponse(), addEffect(ctx, tx, scope)
				})
				if err == nil {
					err = tx.Commit(f.ctx)
				}
				competitorResult <- answer{response, err}
			}()
			// The second atomic claim MUST be waiting behind the uncommitted first claim.
			awaitBlocking(t, f, competitor.PgConn().PID(), owner.PgConn().PID())
			if _, err := f.admin.Exec(f.ctx, "SELECT pg_advisory_unlock($1)", gate); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-ownerResult:
				if err != nil {
					t.Fatal("owner completion", err)
				}
			case <-f.ctx.Done():
				t.Fatal("owner completion timeout")
			}
			select {
			case result := <-competitorResult:
				if changed {
					requireProblem(t, result.err, 422, "idempotency_key_reused")
				} else if result.err != nil || !reflect.DeepEqual(result.response, testResponse()) {
					t.Fatalf("concurrent exactreplay:%+v %v", result.response, result.err)
				}
			case <-f.ctx.Done():
				t.Fatal("competitor completion timeout")
			}
			if n := effectCount(t, f, scope); n != 1 {
				t.Fatalf("duplicate domainwrites:%d", n)
			}
		})
	}
}
func awaitBlocking(t *testing.T, f *pgFixture, waiter, blocker uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	for {
		var witness bool
		err := f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND $2=ANY(pg_blocking_pids(pid)))`, int32(waiter), int32(blocker)).Scan(&witness)
		if err != nil {
			t.Fatalf("lock witness unavailable:%v", err)
		}
		if witness {
			return
		}
		if ctx.Err() != nil {
			t.Fatal("required lock overlap not observed")
		}
		runtime.Gosched()
	}
}

// Cleanup remains bounded even when the command context was canceled.
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
func closeConnection(c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Close(ctx)
}
func TestPGIdempotencyTenantRunnerPanicRollback(t *testing.T) {
	f := fixture(t)
	scope := f.scope("panic")
	hash := Fingerprint("POST", "/test", nil, nil, nil)
	store := localResponses(t)
	sentinel := "domain panic"
	func() {
		defer func() {
			if recovered := recover(); recovered != sentinel {
				t.Fatalf("panic recovery: %v", recovered)
			}
		}()
		err := tenanttx.Run(f.ctx, scope.TenantID, func(ctx context.Context) (pgx.Tx, error) { return f.admin.Begin(ctx) }, func(ctx context.Context, tx pgx.Tx, tenant string) error {
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", tenant); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime")
			return err
		}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ExecuteTx(ctx, tx, scope, hash, time.Hour, store, func(ctx context.Context, tx pgx.Tx) (Response, error) {
				if err := addEffect(ctx, tx, scope); err != nil {
					return Response{}, err
				}
				panic(sentinel)
			})
			return err
		})
		t.Fatalf("panic did not propagate: %v", err)
	}()
	if n := effectCount(t, f, scope); n != 0 {
		t.Fatalf("panic left domainwrites:%d", n)
	}
	tx := f.tx(t, scope.TenantID)
	result, err := ClaimTx(f.ctx, tx, scope, hash, time.Hour)
	if err != nil || result.Claim == nil {
		t.Fatalf("panic left request occupied:%v", err)
	}
	_ = tx.Rollback(f.ctx)
}
