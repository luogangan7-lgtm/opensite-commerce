//go:build pgx

// Responsibility: actual Runner/PGTenantSource/PGStore tenant discovery and RLS gates.
// Dependencies: disposable migrated PG18 DATABASE_URL, pgxadapter and outbox;
// called by PG gates. Only uniquely owned test fixtures are ever dispatched.
package db_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/outbox"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type runnerSender func(context.Context, outbox.Delivery) error

func (f runnerSender) Send(ctx context.Context, d outbox.Delivery) error { return f(ctx, d) }

// Filtering is test-fixture isolation only. The wrapped production source still
// traverses its complete controlled SQL pages, including newly inserted tenants.
type ownedTenantSource struct {
	source outbox.PGTenantSource
	owned  map[string]bool
}

func (s ownedTenantSource) ActiveTenants(ctx context.Context, after string, limit int32) ([]string, error) {
	for {
		ids, err := s.source.ActiveTenants(ctx, after, limit)
		if err != nil || len(ids) == 0 {
			return ids, err
		}
		var kept []string
		for _, id := range ids {
			if s.owned[id] {
				kept = append(kept, id)
			}
		}
		if len(kept) > 0 {
			return kept, nil
		}
		after = ids[len(ids)-1]
	}
}

func TestPGOutboxRunnerDiscoversDynamicTenantsWithScopedTransactions(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		if os.Getenv("OUTBOX_REQUIRE_PG") == "1" {
			t.Fatal("OUTBOX_PG_REQUIRED")
		}
		t.Skip("NOT_RUN: disposable migrated PG18 required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	pool, err := pgxadapter.OpenQueuePool(ctx, dsn, "app_outbox_worker")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var role string
	if err := pool.QueryRow(ctx, "SELECT current_user::text").Scan(&role); err != nil || role != "app_outbox_worker" {
		t.Fatalf("wrong effective worker role %q %v", role, err)
	}
	fixtureName := "INT25-runner-" + uuid.NewString()
	owned := map[string]bool{}
	events := map[string]string{}
	cleanup := func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		for tenantID := range owned {
			if _, err := admin.Exec(cleanupCtx, "DELETE FROM app.outbox_events WHERE tenant_id=$1::text::uuid", tenantID); err != nil {
				t.Error(err)
			}
			if _, err := admin.Exec(cleanupCtx, "DELETE FROM app.tenants WHERE id=$1::text::uuid", tenantID); err != nil {
				t.Error(err)
			}
		}
	}
	defer cleanup()
	seed := func() string {
		t.Helper()
		tenantID := uuid.NewString()
		if _, err := admin.Exec(ctx, "INSERT INTO app.tenants(id,name) VALUES($1::text::uuid,$2)", tenantID, fixtureName); err != nil {
			t.Fatal(err)
		}
		owned[tenantID] = true
		var eventID string
		if err := admin.QueryRow(ctx, "INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES($1::text::uuid,'test.event','{\"test\":true}','test:consumer','runner-test') RETURNING id::text", tenantID).Scan(&eventID); err != nil {
			t.Fatal(err)
		}
		events[tenantID] = eventID
		return tenantID
	}
	seed()
	seed()
	sends := map[string]int{}
	r := outbox.Runner{Source: ownedTenantSource{outbox.PGTenantSource{Pool: pool}, owned}, Dispatcher: outbox.Dispatcher{Store: outbox.PGStore{Pool: pool}, Config: outbox.Config{WorkerID: uuid.NewString(), LeaseSeconds: 60, BaseRetrySeconds: 2, MaxRetrySeconds: 8}}, PollInterval: time.Millisecond, CycleTimeout: 5 * time.Second, TenantBatchSize: 1, EventsPerTenant: 1}
	r.Dispatcher.Sender = runnerSender(func(sendCtx context.Context, d outbox.Delivery) error {
		if !owned[d.TenantID] || events[d.TenantID] != d.EventID {
			return errors.New("unowned fixture dispatch")
		}
		var processing, cleared bool
		if err := admin.QueryRow(sendCtx, "SELECT status='processing' AND lease_owner=$2::text::uuid FROM app.outbox_events WHERE tenant_id=$1::text::uuid AND id=$3::text::uuid", d.TenantID, r.Dispatcher.Config.WorkerID, d.EventID).Scan(&processing); err != nil || !processing {
			return errors.New("uncommitted or crossed tenant claim")
		}
		if err := pool.QueryRow(sendCtx, "SELECT nullif(current_setting('app.tenant_id',true),'') IS NULL").Scan(&cleared); err != nil || !cleared {
			return errors.New("tenant context leaked after claim")
		}
		sends[d.TenantID]++
		if len(sends) == 1 {
			seed()
		} // Tenant did not exist when Runner started.
		return nil
	})
	// Cancel after all three real acknowledgements have committed.
	watched := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				watched <- errors.New("runner did not discover dynamic tenant")
				return
			case <-ticker.C:
				var delivered int
				if err := admin.QueryRow(ctx, "SELECT count(*) FROM app.outbox_events e JOIN app.tenants t ON t.id=e.tenant_id WHERE t.name=$1 AND e.status='delivered'", fixtureName).Scan(&delivered); err != nil {
					watched <- err
					cancel()
					return
				}
				if delivered == 3 {
					watched <- nil
					cancel()
					return
				}
			}
		}
	}()
	if err := r.Run(ctx); err != nil {
		cancel()
		<-watched
		t.Fatal(err)
	}
	if err := <-watched; err != nil {
		t.Fatal(err)
	}
	if len(sends) != 3 {
		t.Fatalf("sent tenants %v", sends)
	}
	for _, n := range sends {
		if n != 1 {
			t.Fatalf("duplicated fixture sends %v", sends)
		}
	}
	var cleared bool
	checkCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := pool.QueryRow(checkCtx, "SELECT nullif(current_setting('app.tenant_id',true),'') IS NULL").Scan(&cleared); err != nil || !cleared {
		t.Fatalf("GUC leaked after complete %v %v", cleared, err)
	}
	if _, err := pool.Exec(checkCtx, "SELECT tenant_id FROM app.outbox_events"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("worker unexpectedly has raw cross-tenant SELECT: %v", err)
	}
}
