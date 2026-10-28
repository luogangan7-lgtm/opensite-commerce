// Responsibilities: prove facade admission precedes storage and preview scope grants.
// Dependencies: publish public contract and central HTTP router; callers: Go gates.
// Invariants: URL/body/forged contexts cannot authorize writes or preview queries.
package publish_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/internal/service"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/httpapitest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const cmsTenant = "11111111-1111-4111-8111-111111111111"
const cmsSite = "22222222-2222-4222-8222-222222222222"
const cmsDraft = "33333333-3333-4333-8333-333333333333"
const cmsActor = "44444444-4444-4444-8444-444444444444"

type cmsAuth struct{ actor bool }

func (cmsAuth) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	return ctx, nil
}
func (a cmsAuth) ResolveActor(context.Context, *http.Request, httpapi.RouteSpec) (httpapi.Actor, error) {
	if a.actor {
		return httpapi.Actor{Kind: "user", ID: cmsActor}, nil
	}
	return httpapi.Actor{}, nil
}
func withCMSContext(t *testing.T, actor bool, fn func(context.Context)) {
	withCMSContextFor(t, actor, cmsTenant, fn)
}
func withCMSContextFor(t *testing.T, actor bool, tenant string, fn func(context.Context)) {
	t.Helper()
	r := httpapi.New(cmsAuth{actor}, nil)
	if err := httpapitest.Register(t, r, false, httpapi.RouteSpec{OperationID: "cmsTest", Method: "GET", Pattern: "/t/{tenant_id}", Access: httpapi.AdminOrAPIKey, Scope: "cms:write"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fn(r.Context()); w.WriteHeader(204) })); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/t/"+tenant, nil))
	if rr.Code != 204 {
		t.Fatalf("admission status%d", rr.Code)
	}
}
func TestCMSAdmissionBeforePersistence(t *testing.T) {
	calls := 0
	sentinel := errors.New("queried")
	run := func(context.Context, string, func(context.Context, pgx.Tx) error) error { calls++; return sentinel }
	m, err := publish.NewWriter(publish.WriteConfig{Run: run, ReadRun: run, PreviewKey: []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	valid := contract.Command{Operation: "create_draft", TenantID: cmsTenant, SiteID: cmsSite, Payload: []byte(`{"locale":"en","title":"Site","description":""}`)}
	if _, err := m.Execute(context.Background(), valid); err == nil || calls != 0 {
		t.Fatal("unstamped reached DB")
	}
	withCMSContext(t, false, func(ctx context.Context) {
		if _, err := m.Execute(ctx, valid); err == nil || calls != 0 {
			t.Fatal("no actor reached DB")
		}
	})
	withCMSContext(t, true, func(ctx context.Context) {
		bad := valid
		bad.TenantID = cmsSite
		if _, err := m.Execute(ctx, bad); !errors.Is(err, contract.ErrNotFound) || calls != 0 {
			t.Fatal("cross tenant", err, calls)
		}
		bad = valid
		bad.Payload = []byte(`{"locale":"en","title":"Site","description":"","private":true}`)
		if _, err := m.Execute(ctx, bad); !errors.Is(err, contract.ErrInvalid) || calls != 0 {
			t.Fatal("unknown field", err, calls)
		}
		if _, err := m.Execute(ctx, valid); !errors.Is(err, sentinel) || calls != 1 {
			t.Fatal("valid admission", err, calls)
		}
	})
}
func TestPreviewVerificationBeforePersistence(t *testing.T) {
	now := time.Unix(1800000000, 0)
	key := []byte(strings.Repeat("k", 32))
	calls := 0
	sentinel := errors.New("preview snapshot queried")
	m, err := publish.NewWriter(publish.WriteConfig{PreviewKey: key, Now: func() time.Time { return now }, PreviewRun: func(_ context.Context, c contract.PreviewClaims, _ func(context.Context, pgx.Tx) error) error {
		calls++
		if c.TenantID != cmsTenant || c.Revision != 9 {
			t.Fatal("scope not verified")
		}
		return sentinel
	}})
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := service.NewPreviewSigner(key, func() time.Time { return now })
	token, expires, err := signer.Issue(cmsTenant, cmsSite, cmsDraft, 9, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Preview(context.Background(), cmsTenant, cmsSite, cmsDraft, token+"x"); !errors.Is(err, contract.ErrPreviewDenied) || calls != 0 {
		t.Fatal("tamper queried", err, calls)
	}
	if _, err := m.Preview(context.Background(), cmsSite, cmsSite, cmsDraft, token); !errors.Is(err, contract.ErrNotFound) || calls != 0 {
		t.Fatal("cross tenant queried", err, calls)
	}
	if _, err := m.Preview(context.Background(), cmsTenant, cmsSite, cmsDraft, token); !errors.Is(err, sentinel) || calls != 1 {
		t.Fatal("verified query missing", err, calls)
	}
	now = expires
	if _, err := m.Preview(context.Background(), cmsTenant, cmsSite, cmsDraft, token); !errors.Is(err, contract.ErrPreviewDenied) || calls != 1 {
		t.Fatal("expired queried", err, calls)
	}
}
