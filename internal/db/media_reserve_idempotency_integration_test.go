//go:build pgx

// Responsibility: exercise media reserve through real HTTP and least-privilege
// tenant transactions. Dependencies: disposable PG fixtures and the media module.
// Invariants: exact response replay, one reservation, scoped keys and atomicity;
// advisory locks plus pg_stat_activity prove concurrent overlap, never sleeps.
package db_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/media"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type mediaReserveAuth struct{ actor string }

func (mediaReserveAuth) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	return ctx, nil
}
func (a mediaReserveAuth) ResolveActor(context.Context, *http.Request, httpapi.RouteSpec) (httpapi.Actor, error) {
	return httpapi.Actor{Kind: "user", ID: a.actor}, nil
}

func mediaReserveRouter(t *testing.T, f *idemPGFixture, actor, label, root string, stores ...idempotency.ResponseStore) *httpapi.Router {
	t.Helper()
	u, err := url.Parse(os.Getenv("C07_MEDIA_DATABASE_URL"))
	if err != nil || u.User == nil || u.User.Username() != "c07_media" {
		t.Fatal("media reserve gate requires the dedicated c07_media login")
	}
	q := u.Query()
	q.Set("application_name", label)
	u.RawQuery = q.Encode()
	runner, err := pgxadapter.OpenQueryRunner(f.ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runner.Close)
	config := media.Config{Run: runner.Queries, Root: root, Idempotency: httpapi.CommandConfig{Run: runner.RunAuthorizedTx}}
	if len(stores) > 0 {
		config.Idempotency.Store = stores[0]
	}
	s := media.New(config)
	r := httpapi.New(mediaReserveAuth{actor}, nil)
	for _, route := range s.Routes() {
		if err := r.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func mediaReserveCall(ctx context.Context, r *httpapi.Router, tenant, key, query, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v1/t/"+tenant+"/media/uploads"+query, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type unavailableMediaResponseStore struct{}

func (unavailableMediaResponseStore) Put(context.Context, idempotency.Response) (string, error) {
	return "", errors.New("injected response persistence failure")
}
func (unavailableMediaResponseStore) Get(context.Context, string) (idempotency.Response, error) {
	return idempotency.Response{}, errors.New("injected response persistence failure")
}

func TestPGMediaReserveIdempotency(t *testing.T) {
	f := idemFixture(t)
	t.Cleanup(func() {
		for _, tenant := range f.tenants {
			_, err := f.admin.Exec(context.Background(), "DELETE FROM app.media_uploads WHERE tenant_id=$1", idemParseUUID(tenant))
			if err != nil {
				t.Error(err)
			}
		}
	})
	root := t.TempDir()
	r := mediaReserveRouter(t, f, f.actor, "media_idem_serial", root)
	body := `{"filename":"idem.png","size_bytes":1,"sha256":"` + strings.Repeat("a", 64) + `","declared_mime":"image/png"}`
	reservationCount := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM app.media_uploads WHERE tenant_id=$1", idemParseUUID(f.tenants[0])).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	count := func(t *testing.T, want int) {
		t.Helper()
		if n := reservationCount(t); n != want {
			t.Fatalf("reservation count=%d want=%d", n, want)
		}
	}
	t.Run("exact_replay_and_restart", func(t *testing.T) {
		before := reservationCount(t)
		key := uuid.NewString()
		first := mediaReserveCall(f.ctx, r, f.tenants[0], key, "?b=2&a=1", body)
		second := mediaReserveCall(f.ctx, r, f.tenants[0], key, "?a=1&b=2", body)
		if first.Code != 201 || second.Code != 201 || !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) || first.Header().Get("Content-Type") != second.Header().Get("Content-Type") {
			t.Errorf("exact replay: first=%d %s second=%d %s", first.Code, first.Body, second.Code, second.Body)
		}
		count(t, before+1)
		// A fresh service and local response store must use the durable receipt.
		restarted := mediaReserveRouter(t, f, f.actor, "media_idem_restart", root)
		again := mediaReserveCall(f.ctx, restarted, f.tenants[0], strings.ToUpper(key), "?a=1&b=2", body)
		if again.Code != 201 || !bytes.Equal(first.Body.Bytes(), again.Body.Bytes()) {
			t.Fatalf("restart replay=%d %s", again.Code, again.Body)
		}
	})
	t.Run("changed_request_rejected", func(t *testing.T) {
		before := reservationCount(t)
		key := uuid.NewString()
		first := mediaReserveCall(f.ctx, r, f.tenants[0], key, "", body)
		if first.Code != 201 {
			t.Fatalf("reserve=%d %s", first.Code, first.Body)
		}
		for _, changed := range []struct{ query, body string }{{"", strings.Replace(body, "idem.png", "other.png", 1)}, {"?changed=1", body}, {"", body + " "}} {
			w := mediaReserveCall(f.ctx, r, f.tenants[0], key, changed.query, changed.body)
			if w.Code != 422 || !strings.Contains(w.Body.String(), `"code":"idempotency_key_reused"`) {
				t.Errorf("changed request=%d %s", w.Code, w.Body)
			}
		}
		count(t, before+1)
	})
	t.Run("scope_isolation", func(t *testing.T) {
		before := reservationCount(t)
		key := uuid.NewString()
		otherActor := mediaReserveRouter(t, f, uuid.NewString(), "media_idem_actor", root)
		for _, tc := range []struct {
			router *httpapi.Router
			tenant string
		}{{r, f.tenants[0]}, {r, f.tenants[1]}, {otherActor, f.tenants[0]}} {
			w := mediaReserveCall(f.ctx, tc.router, tc.tenant, key, "", body)
			if w.Code != 201 {
				t.Fatalf("scoped reserve=%d %s", w.Code, w.Body)
			}
		}
		count(t, before+2)
	})
	t.Run("concurrent_same_key", func(t *testing.T) {
		before := reservationCount(t)
		// Only the first backend parks after inserting an uncommitted upload.
		// The contender therefore runs while the original command is unfinished.
		gate := int64(164)
		label := "media_idem_owner_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		fn := "media_idem_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		_, err := f.admin.Exec(f.ctx, fmt.Sprintf(`CREATE FUNCTION public.%s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF current_setting('application_name')='%s' THEN PERFORM pg_advisory_xact_lock(164); END IF;
 RETURN NEW; END $$;
 CREATE TRIGGER %s AFTER INSERT ON app.media_uploads FOR EACH ROW EXECUTE FUNCTION public.%s();`, fn, label, fn, fn))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = f.admin.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON app.media_uploads; DROP FUNCTION public.%s();", fn, fn))
		})
		if _, err := f.admin.Exec(f.ctx, "SELECT pg_advisory_lock($1)", gate); err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = f.admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", gate) }()
		owner := mediaReserveRouter(t, f, f.actor, label, root)
		key := uuid.NewString()
		finished := make(chan *httptest.ResponseRecorder, 1)
		go func() { finished <- mediaReserveCall(f.ctx, owner, f.tenants[0], key, "", body) }()
		deadline, stop := context.WithTimeout(f.ctx, 10*time.Second)
		defer stop()
		for {
			var parked bool
			if err := f.admin.QueryRow(deadline, "SELECT EXISTS(SELECT FROM pg_stat_activity WHERE application_name=$1 AND wait_event='advisory' AND $2=ANY(pg_blocking_pids(pid)))", label, int32(f.admin.PgConn().PID())).Scan(&parked); err != nil {
				t.Fatal(err)
			}
			if parked {
				break
			}
			runtime.Gosched()
		}
		contender := mediaReserveCall(deadline, r, f.tenants[0], key, "", body)
		if contender.Code != 409 || !strings.Contains(contender.Body.String(), `"code":"idempotency_in_progress"`) {
			t.Errorf("unfinished original must hold claim: %d %s", contender.Code, contender.Body)
		}
		if _, err := f.admin.Exec(f.ctx, "SELECT pg_advisory_unlock($1)", gate); err != nil {
			t.Fatal(err)
		}
		var first *httptest.ResponseRecorder
		select {
		case first = <-finished:
		case <-f.ctx.Done():
			t.Fatal(f.ctx.Err())
		}
		again := mediaReserveCall(f.ctx, r, f.tenants[0], key, "", body)
		if first.Code != 201 || again.Code != 201 || !bytes.Equal(first.Body.Bytes(), again.Body.Bytes()) {
			t.Errorf("concurrent replay: first=%d %s retry=%d %s", first.Code, first.Body, again.Code, again.Body)
		}
		count(t, before+1)
	})
	t.Run("response_failure_rolls_back_upload", func(t *testing.T) {
		before := reservationCount(t)
		failed := mediaReserveRouter(t, f, f.actor, "media_idem_store_failure", root, unavailableMediaResponseStore{})
		key := uuid.NewString()
		w := mediaReserveCall(f.ctx, failed, f.tenants[0], key, "", body)
		if w.Code != 503 {
			t.Fatalf("response persistence failure=%d %s", w.Code, w.Body)
		}
		count(t, before)
		var state string
		if err := f.admin.QueryRow(f.ctx, "SELECT state FROM app.idempotency_records WHERE tenant_id=$1 AND idempotency_key=$2", idemParseUUID(f.tenants[0]), key).Scan(&state); err != nil || state != "in_progress" {
			t.Fatalf("failed response must not complete: state=%s err=%v", state, err)
		}
	})
}
