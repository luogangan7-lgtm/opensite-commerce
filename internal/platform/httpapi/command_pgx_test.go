//go:build pgx

// Responsibilities: verify C-04 transport admission against a disposable PG18.
// Dependencies: real tenanttx adapter and durable response blobs; caller: pgx
// gate. Actor auth is a test substitute; all claim/domain writes use real SQL.
package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type commandTestAuth struct{}

func (commandTestAuth) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	return ctx, nil
}
func (commandTestAuth) ResolveActor(_ context.Context, r *http.Request, _ httpapi.RouteSpec) (httpapi.Actor, error) {
	return httpapi.Actor{Kind: "user", ID: r.Header.Get("X-Test-Actor")}, nil
}

type failingCommandStore struct {
	idempotency.ResponseStore
	fail bool
}

func (s failingCommandStore) Put(ctx context.Context, r idempotency.Response) (string, error) {
	if s.fail {
		return "", errors.New("injected durable response failure")
	}
	return s.ResponseStore.Put(ctx, r)
}

func TestPGCommandActorIsolationReplayAndProcessing(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		if os.Getenv("IDEMPOTENCY_REQUIRE_PG") == "1" {
			t.Fatal("NOT_RUN: DATABASE_URL required")
		}
		t.Skip("NOT_RUN: disposable migrated PG18 required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	runner, closePool, err := pgxadapter.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closePool)
	tenant := uuid.NewString()
	actorA := uuid.NewString()
	actorB := uuid.NewString()
	err = runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO app.tenants(id,name) VALUES($1,'command actor gate')", tenant)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := runner.InMaintenance(cleanup, func(ctx context.Context, tx pgx.Tx) error {
			for _, q := range []string{"DELETE FROM app.idempotency_records WHERE tenant_id=$1", "DELETE FROM app.audit_events WHERE tenant_id=$1", "DELETE FROM app.tenants WHERE id=$1"} {
				if _, e := tx.Exec(ctx, q, tenant); e != nil {
					return e
				}
			}
			return nil
		}); err != nil {
			t.Error(err)
		}
	})
	store, err := idempotency.NewLocalResponseStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
		return runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
				return err
			}
			return fn(ctx, tx)
		})
	}
	spec := httpapi.RouteSpec{OperationID: "createShipment", Method: "POST", Pattern: "/api/v1/t/{tenant_id}/command", Access: httpapi.AdminOrAPIKey, Scope: "fulfillment:write", MaxBodySize: 1024}
	var entered = make(chan struct{})
	var release = make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	config := httpapi.CommandConfig{Run: run, Store: store, TTL: time.Minute}
	makeHandler := func(config httpapi.CommandConfig) http.Handler {
		router := httpapi.New(commandTestAuth{}, nil)
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			session, replay, err := httpapi.BeginCommand(r.Context(), config, "createShipment")
			if err != nil {
				httpapi.WriteProblem(w, r, err)
				return
			}
			if replay != nil {
				_ = httpapi.WriteCommandResponse(w, *replay)
				return
			}
			if r.Header.Get("Idempotency-Key") == "processing" {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					httpapi.WriteProblem(w, r, ctx.Err())
					return
				}
			}
			err = run(r.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, "INSERT INTO app.audit_events(tenant_id,actor_kind,actor_id,operation,resource_type) VALUES($1,'user',$2,'command_gate','shipment')", tenant, r.Header.Get("X-Test-Actor"))
				if err != nil {
					return err
				}
				return session.Complete(ctx, tx, 201, map[string]string{"receipt": session.Key()})
			})
			if err != nil {
				httpapi.WriteProblem(w, r, err)
				return
			}
			_ = httpapi.WriteCommandResponse(w, session.Response())
		})
		if err := router.Register(spec, httpapi.CaptureCommand(spec, handler)); err != nil {
			t.Fatal(err)
		}
		return router
	}
	handler := makeHandler(config)
	request := func(handler http.Handler, actor, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/t/"+tenant+"/command", bytes.NewBufferString(body))
		r = r.WithContext(ctx)
		r.Header.Set("X-Test-Actor", actor)
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("If-Match", "\"2\"")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	first := request(handler, actorA, "same", "{}")
	if first.Code != 201 {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	replay := request(handler, actorA, "same", "{}")
	if replay.Code != first.Code || replay.Body.String() != first.Body.String() || replay.Header().Get("Content-Type") != first.Header().Get("Content-Type") {
		t.Fatalf("replay differs: %d %s", replay.Code, replay.Body.String())
	}
	other := request(handler, actorB, "same", "{}")
	if other.Code != 201 || other.Body.String() == first.Body.String() {
		t.Fatalf("different actor shared receipt: %d %s", other.Code, other.Body.String())
	}
	changed := request(handler, actorA, "same", "{\"changed\":true}")
	if changed.Code != 422 {
		t.Fatalf("changed request: %d %s", changed.Code, changed.Body.String())
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request(handler, actorA, "processing", "{}") }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("processing owner did not enter")
	}
	processing := request(handler, actorA, "processing", "{}")
	if processing.Code != 409 || !bytes.Contains(processing.Body.Bytes(), []byte("idempotency_in_progress")) {
		t.Fatalf("active processing: %d %s", processing.Code, processing.Body.String())
	}
	once.Do(func() { close(release) })
	select {
	case result := <-done:
		if result.Code != 201 {
			t.Fatalf("owner failed: %d %s", result.Code, result.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("owner did not finish")
	}
	broken := config
	broken.Store = failingCommandStore{ResponseStore: store, fail: true}
	failure := request(makeHandler(broken), actorA, "store-failure", "{}")
	if failure.Code != 503 {
		t.Fatalf("store failure: %d %s", failure.Code, failure.Body.String())
	}
	err = runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM app.audit_events WHERE tenant_id=$1", tenant).Scan(&count); err != nil {
			return err
		}
		if count != 3 {
			t.Errorf("completion failure leaked domain write: got%d want3", count)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
