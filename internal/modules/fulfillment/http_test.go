// Responsibilities: exercise fulfillment's strict HTTP admission and commands.
// Dependencies: public service contract and central authenticated routing.
// Invariants: rejected input never runs a transaction; errors expose no storage detail.
package fulfillment

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
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
		{name: "default deny", body: shipmentBody, version: `"2"`, key: "one", want: 401},
		{name: "scope denied", body: shipmentBody, version: `"2"`, key: "one", auth: httpAuth{true}, want: 403},
		{name: "missing version", body: shipmentBody, key: "one", auth: httpAuth{}, want: 428},
		{name: "weak version", body: shipmentBody, version: `W/"2"`, key: "one", auth: httpAuth{}, want: 400},
		{name: "missing key", body: shipmentBody, version: `"2"`, auth: httpAuth{}, want: 400},
		{name: "duplicate version", body: shipmentBody, version: `"2"`, key: "one", auth: httpAuth{}, mutate: func(r *http.Request) { r.Header.Add("If-Match", `"2"`) }, want: 400},
		{name: "duplicate key", body: shipmentBody, version: `"2"`, key: "one", auth: httpAuth{}, mutate: func(r *http.Request) { r.Header.Add("Idempotency-Key", "one") }, want: 400},
		{name: "unknown query", body: shipmentBody, version: `"2"`, key: "one", auth: httpAuth{}, mutate: func(r *http.Request) { r.URL.RawQuery = "other=1" }, want: 400},
		{name: "unknown body proof", body: `{"carrier":"manual","tracking_number":"T1","paid":true,"lines":[]}`, version: `"2"`, key: "one", auth: httpAuth{}, want: 400},
		{name: "foreign tenant", body: shipmentBody, version: `"2"`, key: "one", auth: httpAuth{}, mutate: func(r *http.Request) { r.URL.Path = strings.ReplaceAll(r.URL.Path, httpTenant, httpOrder) }, want: 404},
		{name: "valid command", body: shipmentBody, version: `"2"`, key: "one", auth: httpAuth{}, want: 201, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &httpService{}
			runs := 0
			s := NewHTTP(HTTPConfig{Service: service, Run: func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
				runs++
				return fn(ctx, nil)
			}})
			req := shipmentRequest(tc.body, tc.version, tc.key)
			if tc.mutate != nil {
				tc.mutate(req)
			}
			w := httptest.NewRecorder()
			httpRouter(t, s, tc.auth).ServeHTTP(w, req)
			if w.Code != tc.want || runs != tc.calls || service.calls != tc.calls {
				t.Fatalf("status=%d want=%d runs=%d calls=%d body=%s", w.Code, tc.want, runs, service.calls, w.Body.String())
			}
			if tc.calls == 1 && (service.input.ExpectedVersion != 2 || service.input.IdempotencyKey != "one" || len(service.input.Lines) != 1 || service.input.Lines[0].Quantity != 2) {
				t.Fatalf("input lost: %+v", service.input)
			}
		})
	}
}
func TestHTTPDirectAndRunnerAuthorization(t *testing.T) {
	service := &httpService{}
	s := NewHTTP(HTTPConfig{Service: service, Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
		return fn(context.Background(), nil)
	}})
	_, err := s.CreateShipment(context.Background(), httpapi.CreateShipmentRequestObject{TenantId: uuid.MustParse(httpTenant), OrderId: uuid.MustParse(httpOrder)})
	var problem *apperrors.Error
	if !errors.As(err, &problem) || problem.Status != 401 || service.calls != 0 {
		t.Fatalf("direct handler bypass: %v calls=%d", err, service.calls)
	}
	w := httptest.NewRecorder()
	httpRouter(t, s, httpAuth{}).ServeHTTP(w, shipmentRequest(shipmentBody, `"2"`, "one"))
	if w.Code != 404 || service.calls != 0 {
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
			s := NewHTTP(HTTPConfig{Service: service, Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error { return fn(ctx, nil) }})
			w := httptest.NewRecorder()
			httpRouter(t, s, httpAuth{}).ServeHTTP(w, shipmentRequest(shipmentBody, `"2"`, "one"))
			if w.Code != tc.want || strings.Contains(w.Body.String(), "PRIVATE") {
				t.Fatalf("unsafe error status=%d want=%d %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
