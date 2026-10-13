//go:build pgx

// Responsibility: prove durable customer quotas across tenant and connection boundaries.
// Dependencies: capability-checked customer runner, generated admission queries and owner fixtures.
// Invariants: global/IP budgets are shared, accounts remain tenant-scoped, and observed
// PostgreSQL locks establish overlap; fixture SQL stays inside owner InMaintenance.
package customer_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type customerLimitResult struct {
	delay int64
	err   error
}

// A future global table is optional during RED: these tests fail on admission
// behavior against the old schema, rather than failing on a missing relation.
// Cases are serial and use an expendable CI database, so no customer request
// suite can consume the shared window while this boundary fixture runs.
func customerPGClearGlobalLimits(t *testing.T, f customerPGFixture) {
	t.Helper()
	clear := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return f.owner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DO $fixture$ BEGIN
 IF to_regclass('app.customer_auth_global_limits') IS NOT NULL THEN
  EXECUTE 'TRUNCATE TABLE app.customer_auth_global_limits';
 END IF;
END $fixture$;`)
			return err
		})
	}
	if err := clear(); err != nil {
		t.Fatal("CUSTOMER_PG_GLOBAL_FIXTURE_RESET_FAILED")
	}
	t.Cleanup(func() {
		if err := clear(); err != nil {
			t.Error("CUSTOMER_PG_GLOBAL_FIXTURE_CLEANUP_FAILED")
		}
	})
}

func customerPGNamedLimitRunner(t *testing.T, ctx context.Context, label string) *pgxadapter.CustomerRunner {
	t.Helper()
	dsn, err := url.Parse(os.Getenv("CUSTOMER_AUTH_TEST_DATABASE_URL"))
	if err != nil || (dsn.Scheme != "postgres" && dsn.Scheme != "postgresql") {
		t.Fatal("CUSTOMER_PG_LIMIT_DSN_FORMAT_FAILED")
	}
	query := dsn.Query()
	query.Set("application_name", label)
	dsn.RawQuery = query.Encode()
	runner, err := pgxadapter.OpenCustomerRunner(ctx, dsn.String())
	if err != nil {
		t.Fatal("CUSTOMER_PG_LIMIT_CAPABILITY_CONNECT_FAILED")
	}
	t.Cleanup(runner.Close)
	return runner
}

func customerPGAdmitLimit(ctx context.Context, runner *pgxadapter.CustomerRunner, tenant string, account, ip [32]byte) customerLimitResult {
	var delay int64
	err := runner.InTenant(ctx, tenant, func(q *db.Queries) error {
		var err error
		delay, err = q.AdmitCustomerAuth(ctx, db.AdmitCustomerAuthParams{
			AccountHash: account[:], IpHash: ip[:], Now: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		})
		return err
	})
	return customerLimitResult{delay: delay, err: err}
}

// The first admission retains its runtime transaction while an owner fixture
// connection waits on the controller's advisory lock. PostgreSQL observation,
// rather than a sleep, proves that the first admission still owns its row locks.
// A correct shared limiter then blocks the other tenant's admission on that
// first runtime connection. The old limiter can finish early, yielding the
// semantic two-admitted RED below, without waiting for a synchronization timeout.
func customerPGQuotaBoundary(t *testing.T, f customerPGFixture, firstIP, secondIP [32]byte) (customerLimitResult, customerLimitResult, bool) {
	t.Helper()
	label := "customer_limit_" + uuid.NewString()
	firstLabel, secondLabel, parkLabel := label+"_first", label+"_second", label+"_park"
	firstRunner := customerPGNamedLimitRunner(t, f.ctx, firstLabel)
	secondRunner := customerPGNamedLimitRunner(t, f.ctx, secondLabel)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseGate()
	gateReady, gateDone := make(chan error, 1), make(chan error, 1)
	go func() {
		gateDone <- f.owner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, label)
			gateReady <- err
			if err != nil {
				return err
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case err := <-gateReady:
		if err != nil {
			t.Fatal("CUSTOMER_PG_LIMIT_ADVISORY_GATE_FAILED")
		}
	case <-gateDone:
		t.Fatal("CUSTOMER_PG_LIMIT_ADVISORY_TRANSACTION_FAILED")
	case <-f.ctx.Done():
		t.Fatal("CUSTOMER_PG_LIMIT_ADVISORY_GATE_TIMEOUT")
	}
	firstDone := make(chan customerLimitResult, 1)
	go func() {
		account := sha256.Sum256([]byte(label + "_account_first"))
		var delay int64
		err := firstRunner.InTenant(f.ctx, f.tenant, func(q *db.Queries) error {
			var err error
			delay, err = q.AdmitCustomerAuth(f.ctx, db.AdmitCustomerAuthParams{
				// A caller clock must not reset the server-owned admission window.
				AccountHash: account[:], IpHash: firstIP[:], Now: pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true},
			})
			if err != nil {
				return err
			}
			return f.owner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT set_config('application_name',$1,true)`, parkLabel); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, label)
				return err
			})
		})
		firstDone <- customerLimitResult{delay: delay, err: err}
	}()
	observe := func(sql string, args ...any) (bool, error) {
		var seen bool
		err := f.owner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, sql, args...).Scan(&seen)
		})
		return seen, err
	}
	parked := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		seen, err := observe(`SELECT EXISTS(SELECT FROM pg_stat_activity p,pg_stat_activity f
 WHERE p.application_name=$1 AND p.wait_event='advisory' AND cardinality(pg_blocking_pids(p.pid))>0
 AND f.application_name=$2 AND f.state='idle in transaction')`, parkLabel, firstLabel)
		if err != nil {
			t.Fatal("CUSTOMER_PG_LIMIT_PARK_OBSERVATION_FAILED")
		}
		if seen {
			parked = true
			break
		}
		select {
		case <-firstDone:
			t.Fatal("CUSTOMER_PG_LIMIT_FIRST_DID_NOT_PARK")
		default:
		}
	}
	if !parked {
		t.Fatal("CUSTOMER_PG_LIMIT_PARK_NOT_OBSERVED")
	}
	secondDone := make(chan customerLimitResult, 1)
	go func() {
		account := sha256.Sum256([]byte(label + "_account_second"))
		secondDone <- customerPGAdmitLimit(f.ctx, secondRunner, f.other, account, secondIP)
	}()
	blocked := false
	var second customerLimitResult
	secondReceived := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		seen, err := observe(`SELECT EXISTS(SELECT FROM pg_stat_activity f,pg_stat_activity s
 WHERE f.application_name=$1 AND s.application_name=$2 AND s.wait_event IN('transactionid','tuple')
 AND f.pid=ANY(pg_blocking_pids(s.pid)))`, firstLabel, secondLabel)
		if err != nil {
			t.Fatal("CUSTOMER_PG_LIMIT_CONTENDER_OBSERVATION_FAILED")
		}
		if seen {
			blocked = true
			break
		}
		select {
		case second = <-secondDone:
			secondReceived = true
		default:
		}
		if secondReceived {
			break
		}
	}
	releaseGate()
	if err := <-gateDone; err != nil {
		t.Fatal("CUSTOMER_PG_LIMIT_GATE_RELEASE_FAILED")
	}
	first := <-firstDone
	if !secondReceived {
		second = <-secondDone
	}
	return first, second, blocked
}

// Batch each tenant's typed calls in one transaction so setup cannot consume
// most of the one-minute window merely by committing hundreds of fixtures.
func customerPGPrefillLimits(t *testing.T, f customerPGFixture, count int, fixedIP *[32]byte) {
	t.Helper()
	for offset, tenant := range []string{f.tenant, f.other} {
		err := f.runtime.InTenant(f.ctx, tenant, func(q *db.Queries) error {
			for n := offset; n < count; n += 2 {
				account := sha256.Sum256([]byte(fmt.Sprintf("%s_prefill_account_%d", f.tenant, n)))
				ip := sha256.Sum256([]byte(fmt.Sprintf("%s_prefill_ip_%d", f.tenant, n)))
				if fixedIP != nil {
					ip = *fixedIP
				}
				delay, err := q.AdmitCustomerAuth(f.ctx, db.AdmitCustomerAuthParams{
					AccountHash: account[:], IpHash: ip[:], Now: pgtype.Timestamptz{Time: time.Now(), Valid: true},
				})
				if err != nil {
					return err
				}
				if delay != 0 {
					return errors.New("customer quota prefill was refused")
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal("CUSTOMER_PG_LIMIT_PREFILL_REJECTED")
		}
	}
}

func TestCustomerPGCrossTenantGlobalQuotaBoundary(t *testing.T) {
	f := customerPG(t)
	customerPGClearGlobalLimits(t, f)
	customerPGPrefillLimits(t, f, 299, nil)
	firstIP := sha256.Sum256([]byte(f.tenant + "_global_boundary_ip_first"))
	secondIP := sha256.Sum256([]byte(f.tenant + "_global_boundary_ip_second"))
	first, second, blocked := customerPGQuotaBoundary(t, f, firstIP, secondIP)
	if first.err != nil || second.err != nil {
		t.Fatal("CUSTOMER_PG_GLOBAL_BOUNDARY_DATABASE_FAILED")
	}
	if first.delay != 0 || second.delay <= 0 || second.delay > 60 {
		t.Fatalf("CUSTOMER_PG_CROSS_TENANT_GLOBAL_RED: expected one admission at300 and one refusal at301, got %d/%d", first.delay, second.delay)
	}
	if !blocked {
		t.Fatal("CUSTOMER_PG_GLOBAL_SHARED_LOCK_NOT_OBSERVED")
	}
}

func TestCustomerPGCrossTenantIPQuotaBoundary(t *testing.T) {
	f := customerPG(t)
	customerPGClearGlobalLimits(t, f)
	ip := sha256.Sum256([]byte(f.tenant + "_same_logical_ip_prefix"))
	customerPGPrefillLimits(t, f, 29, &ip)
	first, second, blocked := customerPGQuotaBoundary(t, f, ip, ip)
	if first.err != nil || second.err != nil {
		t.Fatal("CUSTOMER_PG_IP_BOUNDARY_DATABASE_FAILED")
	}
	if first.delay != 0 || second.delay <= 0 || second.delay > 60 {
		t.Fatalf("CUSTOMER_PG_CROSS_TENANT_IP_RED: expected one admission at30 and one refusal at31, got %d/%d", first.delay, second.delay)
	}
	if !blocked {
		t.Fatal("CUSTOMER_PG_IP_SHARED_LOCK_NOT_OBSERVED")
	}
}

func TestCustomerPGAccountCooldownIsTenantScoped(t *testing.T) {
	f := customerPG(t)
	customerPGClearGlobalLimits(t, f)
	account := sha256.Sum256([]byte(f.email))
	ipA, ipB := sha256.Sum256([]byte(f.tenant + "_account_ip")), sha256.Sum256([]byte(f.other + "_account_ip"))
	result := customerPGAdmitLimit(f.ctx, f.runtime, f.tenant, account, ipA)
	if result.err != nil || result.delay != 0 {
		t.Fatal("CUSTOMER_PG_ACCOUNT_FIRST_ADMISSION_FAILED")
	}
	err := f.runtime.InTenant(f.ctx, f.tenant, func(q *db.Queries) error {
		for n := 0; n < 5; n++ {
			recorded, err := q.FailedCustomerAuth(f.ctx, db.FailedCustomerAuthParams{AccountHash: account[:], IpHash: ipA[:]})
			if err != nil {
				return err
			}
			if !recorded {
				return errors.New("customer failure fixture was not recorded")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal("CUSTOMER_PG_ACCOUNT_FAILURE_FIXTURE_FAILED")
	}
	// Change IP so the account cooldown, independently of the IP cooldown,
	// causes tenant A's refusal. The same opaque account bytes in B stay eligible.
	result = customerPGAdmitLimit(f.ctx, f.runtime, f.tenant, account, ipB)
	if result.err != nil || result.delay <= 0 || result.delay > 60 {
		t.Fatal("CUSTOMER_PG_ACCOUNT_COOLDOWN_MISSING")
	}
	result = customerPGAdmitLimit(f.ctx, f.runtime, f.other, account, ipB)
	if result.err != nil || result.delay != 0 {
		t.Fatal("CUSTOMER_PG_ACCOUNT_CROSS_TENANT_LOCKOUT")
	}
}
