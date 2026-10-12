//go:build pgx

// Real queue/inventory contract gates use runtime/worker role pools and an admin
// fixture connection. They are called by go test -tags pgx; fixtures belong to a
// disposable database and immutable inventory history remains until teardown.
package db_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	jobs "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/jobs"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const jobsTestKey = "jobs-pg-test-key-32-bytes-minimum!!"

type jobsPG struct {
	t                                            *testing.T
	ctx                                          context.Context
	admin, runtime, worker                       *pgxpool.Pool
	signer                                       *jobs.Signer
	enqueue                                      *jobs.Enqueuer
	tenant, other, reservation, otherReservation string
}

func jobsRolePool(t *testing.T, ctx context.Context, role, name string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		if os.Getenv("JOBS_REQUIRE_PG") == "1" {
			t.Fatal("DATABASE_URL required")
		}
		t.Skip("NOT_RUN: DATABASE_URL absent")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 8
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	if role != "" {
		cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			if _, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
				return err
			}
			var actual string
			if err := c.QueryRow(ctx, "SELECT current_user").Scan(&actual); err != nil {
				return err
			}
			if actual != role {
				return fmt.Errorf("role mismatch: %s", actual)
			}
			return nil
		}
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if err := p.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	return p
}

func newJobsPG(t *testing.T) *jobsPG {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	t.Cleanup(cancel)
	f := &jobsPG{t: t, ctx: ctx}
	f.admin = jobsRolePool(t, ctx, "", "c04b-jobs-fixture")
	f.runtime = jobsRolePool(t, ctx, "app_runtime", "c04b-jobs-runtime")
	f.worker = jobsRolePool(t, ctx, "app_queue_worker", "c04b-jobs-worker")
	var err error
	f.signer, err = jobs.NewSigner([]byte(jobsTestKey))
	if err != nil {
		t.Fatal(err)
	}
	f.enqueue, err = jobs.NewEnqueuer(f.signer)
	if err != nil {
		t.Fatal(err)
	}
	f.tenant, f.reservation = f.seed()
	f.other, f.otherReservation = f.seed()
	// No history deletion or disabled triggers: the parent removes this entire
	// throwaway container. Remove only queued jobs so later tests cannot run them.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := f.admin.Exec(ctx, "DELETE FROM queue.river_job WHERE args->>'tenant_id' IN ($1,$2)", f.tenant, f.other)
		if err != nil {
			t.Errorf("fixture job cleanup: %v", err)
		}
	})
	return f
}

func (f *jobsPG) seed() (string, string) {
	f.t.Helper()
	tx, err := f.admin.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var tenant, product, sku, res string
	for _, q := range []struct {
		sql  string
		args []any
		dest *string
	}{
		{"INSERT INTO app.tenants(name) VALUES('C04b jobs PG fixture') RETURNING id::text", nil, &tenant},
	} {
		if err := tx.QueryRow(f.ctx, q.sql, q.args...).Scan(q.dest); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := tx.QueryRow(f.ctx, "INSERT INTO app.products(tenant_id,product_code) VALUES($1::text::uuid,'jobs-test') RETURNING id::text", tenant).Scan(&product); err != nil {
		f.t.Fatal(err)
	}
	if err := tx.QueryRow(f.ctx, "INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES($1::text::uuid,$2::text::uuid,'jobs-test-sku','default') RETURNING id::text", tenant, product).Scan(&sku); err != nil {
		f.t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id',$1,true)", tenant); err != nil {
		f.t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, "SET LOCAL ROLE app_inventory_manager"); err != nil {
		f.t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, "SELECT app.inventory_adjust($1::text::uuid,5,'jobs PG fixture',uuidv7(),'inbound')", sku); err != nil {
		f.t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		f.t.Fatal(err)
	}
	if err := tx.QueryRow(f.ctx, "SELECT (app.inventory_reserve($1::text::uuid,1,'checkout',uuidv7(),'jobs-expire',900)).id::text", sku).Scan(&res); err != nil {
		f.t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.admin.Exec(f.ctx, "UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id=$1::text::uuid AND id=$2::text::uuid", tenant, res); err != nil {
		f.t.Fatal(err)
	}
	return tenant, res
}

func (f *jobsPG) tx(tenant string) pgx.Tx {
	f.t.Helper()
	tx, err := f.runtime.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id',$1,true)", tenant); err != nil {
		f.t.Fatal(err)
	}
	return tx
}

func (f *jobsPG) insert(tenant string) int64 {
	f.t.Helper()
	tx := f.tx(tenant)
	result, err := f.enqueue.InventoryExpireTx(f.ctx, tx, 1)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	return result.Job.ID
}

func (f *jobsPG) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.admin.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *jobsPG) inventory(tenant, res, state string, expires int) {
	f.t.Helper()
	var got string
	var reserved, onHand int64
	if err := f.admin.QueryRow(f.ctx, "SELECT r.state,s.reserved,s.on_hand FROM app.inventory_reservations r JOIN app.stock_levels s USING(tenant_id,warehouse_id,sku_id) WHERE r.tenant_id=$1::text::uuid AND r.id=$2::text::uuid", tenant, res).Scan(&got, &reserved, &onHand); err != nil {
		f.t.Fatal(err)
	}
	wantReserved := int64(1)
	if state == "expired" {
		wantReserved = 0
	}
	if got != state || reserved != wantReserved || onHand != 5 {
		f.t.Fatalf("inventory state=%s reserved=%d on_hand=%d", got, reserved, onHand)
	}
	if n := f.count("SELECT count(*) FROM app.inventory_movements WHERE tenant_id=$1::text::uuid AND reservation_id=$2::text::uuid AND kind='expire'", tenant, res); n != expires {
		f.t.Fatalf("expiry effects=%d want=%d", n, expires)
	}
}

func jobsAwait(ctx context.Context, test func() (bool, error)) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := test()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (f *jobsPG) state(id int64, want string, timeout time.Duration) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, timeout)
	defer cancel()
	var state string
	err := jobsAwait(ctx, func() (bool, error) {
		err := f.admin.QueryRow(ctx, "SELECT state::text FROM queue.river_job WHERE id=$1", id).Scan(&state)
		return state == want, err
	})
	if err != nil {
		f.t.Fatalf("job %d state=%s want=%s: %v", id, state, want, err)
	}
}

func (f *jobsPG) start() *river.Client[pgx.Tx] {
	f.t.Helper()
	c, err := jobs.NewWorkerWithService(f.worker, f.signer, jobs.ControlledInventory{Pool: f.worker}, jobs.WorkerConfig{Batch: 1}, river.Config{TestOnly: true, FetchCooldown: 10 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond, JobTimeout: time.Second, RescueStuckJobsAfter: 2 * time.Second})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := c.Start(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.StopAndCancel(ctx); err != nil {
			f.t.Errorf("stop worker: %v", err)
		}
	})
	return c
}

func TestJobsPGTransactionVisibility(t *testing.T) {
	f := newJobsPG(t)
	tx := f.tx(f.tenant)
	r, err := f.enqueue.InventoryExpireTx(f.ctx, tx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT count(*) FROM queue.river_job WHERE id=$1", r.Job.ID); n != 0 {
		t.Fatal("uncommitted job visible")
	}
	if err := tx.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT count(*) FROM queue.river_job WHERE id=$1", r.Job.ID); n != 0 {
		t.Fatal("rolled-back job persisted")
	}
	id := f.insert(f.tenant)
	if n := f.count("SELECT count(*) FROM queue.river_job WHERE id=$1 AND args->>'tenant_id'=$2", id, f.tenant); n != 1 {
		t.Fatal("committed job missing or wrong tenant")
	}
}

func TestJobsPGEnqueueErrorExplicitRollback(t *testing.T) {
	f := newJobsPG(t)
	for _, forceSQL := range []bool{false, true} {
		t.Run(fmt.Sprint(forceSQL), func(t *testing.T) {
			tx := f.tx(f.tenant)
			marker := fmt.Sprintf("error-marker-%t", forceSQL)
			if _, err := tx.Exec(f.ctx, "INSERT INTO app.products(tenant_id,product_code) VALUES($1::text::uuid,$2)", f.tenant, marker); err != nil {
				t.Fatal(err)
			}
			batch := 0
			if forceSQL {
				batch = 1
				if _, err := tx.Exec(f.ctx, "SET LOCAL ROLE app_outbox_worker"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.enqueue.InventoryExpireTx(f.ctx, tx, batch); err == nil {
				t.Fatal("expected enqueue failure")
			}
			if err := tx.Commit(f.ctx); !errors.Is(err, pgx.ErrTxClosed) {
				t.Fatalf("producer did not explicitly close failed transaction: %v", err)
			}
			if n := f.count("SELECT count(*) FROM app.products WHERE tenant_id=$1::text::uuid AND product_code=$2", f.tenant, marker); n != 0 {
				t.Fatal("failed enqueue committed domain write")
			}
		})
	}
}

func TestJobsPGQueueRLSAndNoDDL(t *testing.T) {
	f := newJobsPG(t)
	own, other := f.insert(f.tenant), f.insert(f.other)
	tx := f.tx(f.tenant)
	var ownCount, otherCount int
	if err := tx.QueryRow(f.ctx, "SELECT count(*) FROM queue.river_job WHERE id=$1", own).Scan(&ownCount); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(f.ctx, "SELECT count(*) FROM queue.river_job WHERE id=$1", other).Scan(&otherCount); err != nil {
		t.Fatal(err)
	}
	if ownCount != 1 || otherCount != 0 {
		t.Fatalf("queue tenant visibility own=%d other=%d", ownCount, otherCount)
	}
	_ = tx.Rollback(f.ctx)
	tx = f.tx(f.tenant)
	forged := jobs.InventoryExpireArgs{TenantID: f.other, Batch: 1}
	h := hmac.New(sha256.New, []byte(jobsTestKey))
	fmt.Fprintf(h, "opensite-job-v1\x00%s\x00%s\x00%d", jobs.InventoryExpireKind, forged.TenantID, forged.Batch)
	forged.Proof = hex.EncodeToString(h.Sum(nil))
	rawClient, err := river.NewClient(riverpgxv5.New(nil), &river.Config{Schema: "queue"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rawClient.InsertTx(f.ctx, tx, forged, nil)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("foreign-tenant insert not denied by RLS: %v", err)
	}
	_ = tx.Rollback(f.ctx)
	for _, pool := range []*pgxpool.Pool{f.runtime, f.worker} {
		tx, err := pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(f.ctx, "CREATE TABLE queue.c04b_jobs_forbidden_probe(id int)")
		_ = tx.Rollback(f.ctx)
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("queue DDL not denied: %v", err)
		}
	}
}

func TestJobsPGForgedTenantCancelledWithoutEffect(t *testing.T) {
	f := newJobsPG(t)
	id := f.insert(f.tenant)
	if _, err := f.admin.Exec(f.ctx, "UPDATE queue.river_job SET args=jsonb_set(args,'{tenant_id}',to_jsonb($1::text)) WHERE id=$2", f.other, id); err != nil {
		t.Fatal(err)
	}
	f.start()
	f.state(id, "cancelled", 15*time.Second)
	var attempt int
	if err := f.admin.QueryRow(f.ctx, "SELECT attempt FROM queue.river_job WHERE id=$1", id).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	if attempt != 1 {
		t.Fatalf("forged job retried %d times", attempt)
	}
	f.inventory(f.tenant, f.reservation, "active", 0)
	f.inventory(f.other, f.otherReservation, "active", 0)
}

func TestJobsPGDuplicateExpiryHasSingleEffect(t *testing.T) {
	f := newJobsPG(t)
	first, second := f.insert(f.tenant), f.insert(f.tenant)
	if first == second {
		t.Fatal("test requires two real jobs")
	}
	f.start()
	f.state(first, "completed", 15*time.Second)
	f.state(second, "completed", 15*time.Second)
	f.inventory(f.tenant, f.reservation, "expired", 1)
	f.inventory(f.other, f.otherReservation, "active", 0)
}

func TestJobsPGSameDeliveryTwiceHasSingleEffect(t *testing.T) {
	f := newJobsPG(t)
	id := f.insert(f.tenant)
	var raw []byte
	if err := f.admin.QueryRow(f.ctx, "SELECT args FROM queue.river_job WHERE id=$1", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var args jobs.InventoryExpireArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	job := &river.Job[jobs.InventoryExpireArgs]{JobRow: &rivertype.JobRow{ID: id, Kind: jobs.InventoryExpireKind}, Args: args}
	w := &jobs.InventoryWorker{Signer: f.signer, Service: jobs.ControlledInventory{Pool: f.worker}}
	for range 2 {
		if err := w.Work(f.ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	f.inventory(f.tenant, f.reservation, "expired", 1)
	f.inventory(f.other, f.otherReservation, "active", 0)
}

func TestJobsPGRestrictedLoginCannotEscalate(t *testing.T) {
	f := newJobsPG(t)
	for _, role := range []string{"app_runtime", "app_queue_worker"} {
		name := "c04b_login_" + role + "_" + f.tenant[:8]
		// Random fixture-only credentials stay in parameters, never stdout or argv.
		password := f.tenant + f.reservation
		if _, err := f.admin.Exec(f.ctx, "CREATE ROLE "+pgx.Identifier{name}.Sanitize()+" LOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := f.admin.Exec(f.ctx, "DROP ROLE "+pgx.Identifier{name}.Sanitize()); err != nil {
				t.Error(err)
			}
		}()
		// ALTER ROLE does not accept parameter placeholders. Use PostgreSQL's own
		// literal quoting to construct this test-only credential statement.
		var statement string
		if err := f.admin.QueryRow(f.ctx, "SELECT format('ALTER ROLE %I PASSWORD %L',$1::text,$2::text)", name, password).Scan(&statement); err != nil {
			t.Fatal(err)
		}
		if _, err := f.admin.Exec(f.ctx, statement); err != nil {
			t.Fatal("fixture credential setup failed")
		}
		if _, err := f.admin.Exec(f.ctx, "GRANT "+pgx.Identifier{role}.Sanitize()+" TO "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		func() {
			cfg, err := pgx.ParseConfig(os.Getenv("DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			cfg.User, cfg.Password = name, password
			conn, err := pgx.ConnectConfig(f.ctx, cfg)
			if err != nil {
				t.Fatal("restricted login failed")
			}
			defer conn.Close(context.Background())
			if _, err := conn.Exec(f.ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Fatal(err)
			}
			var session, effective string
			if err := conn.QueryRow(f.ctx, "SELECT session_user,current_user").Scan(&session, &effective); err != nil || session != name || effective != role {
				t.Fatalf("session role witness: %s/%s %v", session, effective, err)
			}
			for _, q := range []string{"SET ROLE app_river_migrator", "CREATE TABLE queue.c04b_login_ddl(id int)"} {
				_, err := conn.Exec(f.ctx, q)
				var pgerr *pgconn.PgError
				if !errors.As(err, &pgerr) || pgerr.Code != "42501" {
					t.Fatalf("restricted login escalation accepted: %v", err)
				}
			}
			if role == "app_runtime" {
				_, err := conn.Exec(f.ctx, "SET ROLE app_queue_worker")
				var pgerr *pgconn.PgError
				if !errors.As(err, &pgerr) || pgerr.Code != "42501" {
					t.Fatalf("runtime login acquired worker: %v", err)
				}
			}
		}()
	}
}

func TestJobsPGUnknownKindCancelled(t *testing.T) {
	f := newJobsPG(t)
	id := f.insert(f.tenant)
	if _, err := f.admin.Exec(f.ctx, "UPDATE queue.river_job SET kind='unregistered_example' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	f.start()
	f.state(id, "cancelled", 15*time.Second)
	f.inventory(f.tenant, f.reservation, "active", 0)
}

type jobsBlockingExpirer struct {
	pool *pgxpool.Pool
	key  int64
}

func (s jobsBlockingExpirer) Expire(ctx context.Context, tenant string, batch int) error {
	c, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer c.Release()
	if _, err := c.Exec(ctx, "SELECT pg_advisory_lock($1)", s.key); err != nil {
		return err
	}
	defer c.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", s.key)
	return (jobs.ControlledInventory{Pool: s.pool}).Expire(ctx, tenant, batch)
}

func TestJobsPGCrashHelper(t *testing.T) {
	if os.Getenv("JOBS_CRASH_HELPER") != "1" {
		t.Skip("subprocess only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	key, err := strconv.ParseInt(os.Getenv("JOBS_CRASH_KEY"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	p := jobsRolePool(t, ctx, "app_queue_worker", os.Getenv("JOBS_CRASH_APP"))
	s, err := jobs.NewSigner([]byte(jobsTestKey))
	if err != nil {
		t.Fatal(err)
	}
	c, err := jobs.NewWorkerWithService(p, s, jobsBlockingExpirer{p, key}, jobs.WorkerConfig{Batch: 1}, river.Config{TestOnly: true, FetchCooldown: 10 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond, JobTimeout: 30 * time.Second, RescueStuckJobsAfter: 31 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	t.Fatal("crash helper survived until deadline")
}

func TestJobsPGCrashRestartRescuesRunningJob(t *testing.T) {
	f := newJobsPG(t)
	id := f.insert(f.tenant)
	lock, err := f.admin.Acquire(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	var key int64
	var holder int
	if err := lock.QueryRow(f.ctx, "SELECT hashtextextended($1,0),pg_backend_pid()", f.tenant).Scan(&key, &holder); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(f.ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		t.Fatal(err)
	}
	defer lock.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	app := "c04b-jobs-crash-" + f.tenant
	cmd := exec.Command(executable, "-test.run=^TestJobsPGCrashHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "JOBS_CRASH_HELPER=1", "JOBS_CRASH_KEY="+strconv.FormatInt(key, 10), "JOBS_CRASH_APP="+app)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Logf("crash helper run PID=%d job=%d", cmd.Process.Pid, id)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("crash helper did not exit")
			}
		}
	}()
	witnessCtx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	err = jobsAwait(witnessCtx, func() (bool, error) {
		var witnessed bool
		err := f.admin.QueryRow(witnessCtx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event='advisory' AND $2=ANY(pg_blocking_pids(pid))) AND EXISTS(SELECT 1 FROM queue.river_job WHERE id=$3 AND state='running')", app, holder, id).Scan(&witnessed)
		return witnessed, err
	})
	cancel()
	if err != nil {
		t.Fatalf("no database running/advisory witness: %v", err)
	}
	f.inventory(f.tenant, f.reservation, "active", 0)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		waited = true
		if err == nil {
			t.Fatalf("helper unexpectedly exited successfully: %s", output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("killed helper did not terminate")
	}
	if _, err := lock.Exec(f.ctx, "SELECT pg_advisory_unlock($1)", key); err != nil {
		t.Fatal(err)
	}
	tag, err := f.admin.Exec(f.ctx, "UPDATE queue.river_job SET attempted_at=clock_timestamp()-interval '2 minutes' WHERE id=$1 AND state='running'", id)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("running lease fixture lost: %v rows=%d", err, tag.RowsAffected())
	}
	f.start()
	f.state(id, "completed", 40*time.Second)
	var attempt int
	if err := f.admin.QueryRow(f.ctx, "SELECT attempt FROM queue.river_job WHERE id=$1", id).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	if attempt < 2 {
		t.Fatalf("job never reattempted after crash: %d", attempt)
	}
	f.inventory(f.tenant, f.reservation, "expired", 1)
	f.inventory(f.other, f.otherReservation, "active", 0)
}
