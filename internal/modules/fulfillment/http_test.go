// Responsibilities: exercise fulfillment's strict HTTP admission and commands.
// Dependencies: public service contract and central authenticated routing.
// Invariants: rejected input never runs a transaction; errors expose no storage detail.
package fulfillment

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/httpapitest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
)

const httpTenant = "11111111-1111-4111-8111-111111111111"
const httpOrder = "22222222-2222-4222-8222-222222222222"
const httpLine = "33333333-3333-4333-8333-333333333333"
const httpShipment = "44444444-4444-4444-8444-444444444444"
const shipmentBody = `{"carrier":"manual","tracking_number":"T1","lines":[{"order_line_id":"` + httpLine + `","quantity":2}]}`

type httpAuth struct{ denied bool }

func (a httpAuth) Authenticate(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	if a.denied || spec.Access != httpapi.AdminOrAPIKey || spec.Scope != "fulfillment:write" {
		return nil, apperrors.New(403, "forbidden", "Scope denied")
	}
	if r.PathValue("tenant_id") != httpTenant {
		return nil, apperrors.New(404, "not_found", "Tenant not found")
	}
	return ctx, nil
}

func (a httpAuth) ResolveActor(context.Context, *http.Request, httpapi.RouteSpec) (httpapi.Actor, error) {
	return httpapi.Actor{Kind: "user", ID: httpLine}, nil
}

type alternateActorAuth struct{ httpAuth }

func (alternateActorAuth) ResolveActor(context.Context, *http.Request, httpapi.RouteSpec) (httpapi.Actor, error) {
	return httpapi.Actor{Kind: "user", ID: httpOrder}, nil
}

type legacyHTTPAuth struct{ httpapi.Authenticator }

// This test-only claim mirrors C-04 outcomes; persistence is the real durable
// local response store. Production never uses this injected claim implementation.
type claimRecord struct {
	hash     [32]byte
	response *idempotency.Response
}
type httpClaims struct {
	rows                map[httpapi.CommandScope]*claimRecord
	claims, completions int
	completeErr         error
}

func claimConfig(t *testing.T, c *httpClaims) httpapi.CommandConfig {
	t.Helper()
	store, err := idempotency.NewLocalResponseStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return httpapi.CommandConfig{Store: store, Claim: func(ctx context.Context, tx pgx.Tx, scope httpapi.CommandScope, hash [32]byte, ttl time.Duration, store idempotency.ResponseStore) (httpapi.CommandCompletion, *idempotency.Response, error) {
		c.claims++
		if c.rows == nil {
			c.rows = map[httpapi.CommandScope]*claimRecord{}
		}
		if prior := c.rows[scope]; prior != nil {
			if prior.hash != hash {
				return nil, nil, apperrors.New(422, "idempotency_key_reused", "Different request")
			}
			if prior.response == nil {
				return nil, nil, apperrors.New(409, "idempotency_in_progress", "Request in progress")
			}
			return nil, prior.response, nil
		}
		row := &claimRecord{hash: hash}
		c.rows[scope] = row
		return func(ctx context.Context, completingTx pgx.Tx, response idempotency.Response) error {
			if c.completeErr != nil {
				return c.completeErr
			}
			ref, e := store.Put(ctx, response)
			if e != nil {
				return e
			}
			saved, e := store.Get(ctx, ref)
			if e != nil {
				return e
			}
			c.completions++
			row.response = &saved
			return nil
		}, nil, nil
	}}
}

type httpService struct {
	calls int
	input contract.CreateInput
	err   error
}

func (s *httpService) CreateShipment(_ context.Context, _ pgx.Tx, tenant string, in contract.CreateInput) (contract.Shipment, error) {
	s.calls++
	s.input = in
	if tenant != httpTenant {
		panic("wrong authorized tenant")
	}
	return contract.Shipment{ID: httpShipment, OrderID: httpOrder, OrderVersion: 3, Status: "shipped", Lines: []contract.ShipmentLine{{ID: httpShipment, OrderLineID: httpLine, Quantity: 2}}}, s.err
}
func httpRouter(t *testing.T, s *HTTP, auth httpapi.Authenticator) http.Handler {
	t.Helper()
	r := httpapi.New(auth, nil)
	for _, route := range s.Routes() {
		if err := r.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	return r
}
func shipmentRequest(body, version, key string) *http.Request {
	r := httptest.NewRequest("POST", "/api/v1/t/"+httpTenant+"/orders/"+httpOrder+"/shipments", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if version != "" {
		r.Header.Set("If-Match", version)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	return r
}
func TestHTTPAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, body, version, key string
		auth                     httpapi.Authenticator
		mutate                   func(*http.Request)
		want, calls              int
	}{
		{name: "default deny", body: shipmentBody, version: `"2"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", want: 401},
		{name: "scope denied", body: shipmentBody, version: `"2"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{true}, want: 403},
		{name: "missing version", body: shipmentBody, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, want: 428},
		{name: "weak version", body: shipmentBody, version: `W/"2"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, want: 400},
		{name: "missing key", body: shipmentBody, version: `"2"`, auth: httpAuth{}, want: 400},
		{name: "duplicate version", body: shipmentBody, version: `"2"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, mutate: func(r *http.Request) { r.Header.Add("If-Match", `"2"`) }, want: 400},
		{name: "duplicate key", body: shipmentBody, version: `"2"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, mutate: func(r *http.Request) { r.Header.Add("Idempotency-Key", "b47bb372-4ddf-5f46-bdb1-f20e176b3941") }, want: 400},
		{name: "unknown query", body: shipmentBody, version: `"2"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, mutate: func(r *http.Request) { r.URL.RawQuery = "other=1" }, want: 400},
		{name: "unknown body proof", body: `{"carrier":"manual","tracking_number":"T1","paid":true,"lines":[]}`, version: `"2"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, want: 400},
		{name: "foreign tenant", body: shipmentBody, version: `"2"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, mutate: func(r *http.Request) { r.URL.Path = strings.ReplaceAll(r.URL.Path, httpTenant, httpOrder) }, want: 404},
		{name: "valid command", body: shipmentBody, version: `"2"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, want: 201, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &httpService{}
			runs := 0
			s := NewHTTP(HTTPConfig{Idempotency: claimConfig(t, &httpClaims{}), Service: service, Run: func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
				runs++
				return fn(ctx, nil)
			}})
			req := shipmentRequest(tc.body, tc.version, tc.key)
			if tc.mutate != nil {
				tc.mutate(req)
			}
			w := httptest.NewRecorder()
			httpRouter(t, s, tc.auth).ServeHTTP(w, req)
			if w.Code != tc.want || runs != 2*tc.calls || service.calls != tc.calls {
				t.Fatalf("status=%d want=%d runs=%d calls=%d body=%s", w.Code, tc.want, runs, service.calls, w.Body.String())
			}
			if tc.calls == 1 && (service.input.ExpectedVersion != 2 || len(service.input.IdempotencyKey) != 36 || len(service.input.Lines) != 1 || service.input.Lines[0].Quantity != 2) {
				t.Fatalf("input lost: %+v", service.input)
			}
		})
	}
}
func TestHTTPDirectAndRunnerAuthorization(t *testing.T) {
	service := &httpService{}
	s := NewHTTP(HTTPConfig{Idempotency: claimConfig(t, &httpClaims{}), Service: service, Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
		return fn(context.Background(), nil)
	}})
	_, err := s.CreateShipment(context.Background(), httpapi.CreateShipmentRequestObject{TenantId: uuid.MustParse(httpTenant), OrderId: uuid.MustParse(httpOrder)})
	var problem *apperrors.Error
	if !errors.As(err, &problem) || problem.Status != 401 || service.calls != 0 {
		t.Fatalf("direct handler bypass: %v calls=%d", err, service.calls)
	}
	w := httptest.NewRecorder()
	httpRouter(t, s, httpAuth{}).ServeHTTP(w, shipmentRequest(shipmentBody, `"2"`, "b47bb372-4ddf-5f46-bdb1-f20e176b3941"))
	if w.Code != 401 || service.calls != 0 {
		t.Fatalf("callback lost stamp: %d calls=%d", w.Code, service.calls)
	}
}
func TestHTTPDomainErrorsAreSanitized(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{{contract.ErrNotFound, 404}, {contract.ErrPrecondition, 412}, {contract.ErrKeyReused, 422}, {contract.ErrConflict, 409}, {contract.ErrInvalid, 422}, {contract.ErrRetry, 503}, {contract.ErrUnavailable, 503}, {errors.New("postgres://user:PRIVATE@invalid"), 500}, {apperrors.New(422, "forged", "PRIVATE"), 500}} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			service := &httpService{err: tc.err}
			s := NewHTTP(HTTPConfig{Idempotency: claimConfig(t, &httpClaims{}), Service: service, Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error { return fn(ctx, nil) }})
			w := httptest.NewRecorder()
			httpRouter(t, s, httpAuth{}).ServeHTTP(w, shipmentRequest(shipmentBody, `"2"`, "b47bb372-4ddf-5f46-bdb1-f20e176b3941"))
			if w.Code != tc.want || strings.Contains(w.Body.String(), "PRIVATE") {
				t.Fatalf("unsafe error status=%d want=%d %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestHTTPCommandReplayAndActor(t *testing.T) {
	service := &httpService{}
	claims := &httpClaims{}
	runs := 0
	s := NewHTTP(HTTPConfig{Service: service, Idempotency: claimConfig(t, claims), Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
		runs++
		return fn(ctx, nil)
	}})
	handler := httpRouter(t, s, httpAuth{})
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, shipmentRequest(shipmentBody, `"2"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
	if first.Code != 201 || claims.claims != 1 || claims.completions != 1 || runs != 2 || service.calls != 1 {
		t.Fatalf("protocol not used: status=%d claims=%d completions=%d runs=%d calls=%d", first.Code, claims.claims, claims.completions, runs, service.calls)
	}
	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, shipmentRequest(shipmentBody, `"2"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
	if replay.Code != first.Code || !bytes.Equal(replay.Body.Bytes(), first.Body.Bytes()) || replay.Header().Get("Content-Type") != first.Header().Get("Content-Type") || service.calls != 1 || claims.completions != 1 {
		t.Fatalf("response drift or duplicate command: %d %s", replay.Code, replay.Body.String())
	}
	changed := httptest.NewRecorder()
	handler.ServeHTTP(changed, shipmentRequest(strings.ReplaceAll(shipmentBody, "T1", "T2"), `"2"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
	if changed.Code != 422 || service.calls != 1 {
		t.Fatalf("different body bypass: %d calls=%d", changed.Code, service.calls)
	}
	oldKey := service.input.IdempotencyKey
	otherActor := httptest.NewRecorder()
	httpRouter(t, s, alternateActorAuth{}).ServeHTTP(otherActor, shipmentRequest(shipmentBody, `"2"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
	if otherActor.Code != 201 || service.calls != 2 || service.input.IdempotencyKey == oldKey {
		t.Fatalf("actor scopes collapsed: %d calls=%d", otherActor.Code, service.calls)
	}
	missing := httptest.NewRecorder()
	httpRouter(t, s, legacyHTTPAuth{httpAuth{}}).ServeHTTP(missing, shipmentRequest(shipmentBody, `"2"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
	if missing.Code != 401 || service.calls != 2 {
		t.Fatalf("missing actor bypass: %d calls=%d", missing.Code, service.calls)
	}
}

func TestHTTPActiveClaimAndFailedCompletion(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		domainErr, completeErr error
		first                  int
	}{
		{"interrupted domain command", contract.ErrConflict, nil, 409}, {"response persistence failed", nil, errors.New("PRIVATE response store failure"), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &httpService{err: tc.domainErr}
			claims := &httpClaims{completeErr: tc.completeErr}
			s := NewHTTP(HTTPConfig{Service: service, Idempotency: claimConfig(t, claims), Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error { return fn(ctx, nil) }})
			handler := httpRouter(t, s, httpAuth{})
			first := httptest.NewRecorder()
			handler.ServeHTTP(first, shipmentRequest(shipmentBody, `"2"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
			if first.Code != tc.first || claims.completions != 0 || strings.Contains(first.Body.String(), "PRIVATE") {
				t.Fatalf("failed completion emitted result: %d %s", first.Code, first.Body.String())
			}
			second := httptest.NewRecorder()
			handler.ServeHTTP(second, shipmentRequest(shipmentBody, `"2"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
			if second.Code != 409 || !strings.Contains(second.Body.String(), "idempotency_in_progress") || service.calls != 1 {
				t.Fatalf("active claim ran command: %d calls=%d %s", second.Code, service.calls, second.Body.String())
			}
		})
	}
}

func TestHTTPBusinessTransactionMustRetainActor(t *testing.T) {
	var tenantOnly context.Context
	legacy := httpapi.New(legacyHTTPAuth{httpAuth{}}, nil)
	spec := httpapi.RouteSpec{OperationID: "actorless", Method: "POST", Pattern: "/api/v1/t/{tenant_id}/actorless", Access: httpapi.AdminOrAPIKey, Scope: "fulfillment:write"}
	if err := httpapitest.Register(t, legacy, false, spec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tenantOnly = r.Context(); w.WriteHeader(204) })); err != nil {
		t.Fatal(err)
	}
	legacy.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/t/"+httpTenant+"/actorless", nil))
	service := &httpService{}
	runs := 0
	claims := &httpClaims{}
	s := NewHTTP(HTTPConfig{Service: service, Idempotency: claimConfig(t, claims), Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
		runs++
		if runs == 2 {
			ctx = tenantOnly
		}
		return fn(ctx, nil)
	}})
	w := httptest.NewRecorder()
	httpRouter(t, s, httpAuth{}).ServeHTTP(w, shipmentRequest(shipmentBody, `"2"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
	if w.Code != 404 || service.calls != 0 || claims.completions != 0 {
		t.Fatalf("business tx lost actor: status=%d calls=%d completions=%d", w.Code, service.calls, claims.completions)
	}
}
