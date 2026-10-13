// Responsibilities: verify return commands use strict DTOs and trusted service ports.
// Dependencies: public return/payment contracts and authenticated routing.
// Invariants: client input cannot supply PSP/ledger proof; errors never expose it.
package returns

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

const httpTenant = "11111111-1111-4111-8111-111111111111"
const httpOrder = "22222222-2222-4222-8222-222222222222"
const httpLine = "33333333-3333-4333-8333-333333333333"
const httpReturn = "44444444-4444-4444-8444-444444444444"
const returnBody = `{"kind":"unshipped_cancel","reason":"buyer cancellation","lines":[{"order_line_id":"` + httpLine + `","quantity":1}]}`

type httpAuth struct{ denied bool }

func (a httpAuth) Authenticate(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	if a.denied || spec.Access != httpapi.AdminOrAPIKey || spec.Scope != "returns:write" {
		return nil, apperrors.New(403, "forbidden", "Scope denied")
	}
	if r.PathValue("tenant_id") != httpTenant {
		return nil, apperrors.New(404, "not_found", "Tenant not found")
	}
	return ctx, nil
}

type httpService struct {
	calls   int
	request contract.RequestInput
	receive contract.ReceiveInput
	refund  contract.RefundInput
	err     error
}

func (s *httpService) result(ctx context.Context, tenant string) (contract.Return, error) {
	s.calls++
	if got, e := httpapi.AuthorizedTenant(ctx); e != nil || got != tenant || tenant != httpTenant {
		panic("unauthorized command")
	}
	return contract.Return{ID: httpReturn, OrderID: httpOrder, OrderVersion: 5, Kind: contract.UnshippedCancel, Status: "received", Restocked: true, Lines: []contract.ReturnLine{{ID: httpReturn, OrderLineID: httpLine, Quantity: 1}}}, s.err
}
func (s *httpService) Request(ctx context.Context, _ pgx.Tx, tenant string, in contract.RequestInput) (contract.Return, error) {
	s.request = in
	return s.result(ctx, tenant)
}
func (s *httpService) Receive(ctx context.Context, _ pgx.Tx, tenant string, in contract.ReceiveInput) (contract.Return, error) {
	s.receive = in
	return s.result(ctx, tenant)
}
func (s *httpService) Refund(ctx context.Context, tenant string, in contract.RefundInput) (contract.Return, error) {
	s.refund = in
	return s.result(ctx, tenant)
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
func commandRequest(operation, body, version, key string) *http.Request {
	path := "/api/v1/t/" + httpTenant + "/returns/" + httpReturn + "/" + operation
	if operation == "request" {
		path = "/api/v1/t/" + httpTenant + "/orders/" + httpOrder + "/returns"
	}
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if version != "" {
		r.Header.Set("If-Match", version)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	return r
}
func TestHTTPAdmissionAndCommands(t *testing.T) {
	for _, tc := range []struct {
		name, operation, body, version, key string
		auth                                httpapi.Authenticator
		mutate                              func(*http.Request)
		want, runs, calls                   int
	}{
		{name: "default deny", operation: "request", body: returnBody, version: `"4"`, key: "one", want: 401},
		{name: "scope denied", operation: "refund", body: `{}`, version: `"4"`, key: "one", auth: httpAuth{true}, want: 403},
		{name: "missing version", operation: "receive", body: `{"restock":true}`, key: "one", auth: httpAuth{}, want: 428},
		{name: "weak version", operation: "refund", body: `{}`, version: `W/"4"`, key: "one", auth: httpAuth{}, want: 400},
		{name: "missing key", operation: "request", body: returnBody, version: `"4"`, auth: httpAuth{}, want: 400},
		{name: "duplicate key", operation: "request", body: returnBody, version: `"4"`, key: "one", auth: httpAuth{}, mutate: func(r *http.Request) { r.Header.Add("Idempotency-Key", "one") }, want: 400},
		{name: "unknown query", operation: "refund", body: `{}`, version: `"4"`, key: "one", auth: httpAuth{}, mutate: func(r *http.Request) { r.URL.RawQuery = "paid=1" }, want: 400},
		{name: "client provider proof", operation: "refund", body: `{"provider_ref":"client","amount":9,"cash_account":"client"}`, version: `"4"`, key: "one", auth: httpAuth{}, want: 400},
		{name: "client receive proof", operation: "receive", body: `{"restock":true,"quantity":10}`, version: `"4"`, key: "one", auth: httpAuth{}, want: 400},
		{name: "foreign tenant", operation: "refund", body: `{}`, version: `"4"`, key: "one", auth: httpAuth{}, mutate: func(r *http.Request) { r.URL.Path = strings.ReplaceAll(r.URL.Path, httpTenant, httpOrder) }, want: 404},
		{name: "request", operation: "request", body: returnBody, version: `"4"`, key: "one", auth: httpAuth{}, want: 201, runs: 1, calls: 1},
		{name: "receive", operation: "receive", body: `{"restock":true}`, version: `"4"`, key: "one", auth: httpAuth{}, want: 200, runs: 1, calls: 1},
		{name: "refund", operation: "refund", body: `{}`, version: `"4"`, key: "one", auth: httpAuth{}, want: 200, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &httpService{}
			runs := 0
			s := NewHTTP(HTTPConfig{Service: service, Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
				runs++
				return fn(ctx, nil)
			}})
			r := commandRequest(tc.operation, tc.body, tc.version, tc.key)
			if tc.mutate != nil {
				tc.mutate(r)
			}
			w := httptest.NewRecorder()
			httpRouter(t, s, tc.auth).ServeHTTP(w, r)
			if w.Code != tc.want || runs != tc.runs || service.calls != tc.calls {
				t.Fatalf("status=%d want=%d runs=%d want=%d calls=%d body=%s", w.Code, tc.want, runs, tc.runs, service.calls, w.Body.String())
			}
			if tc.calls > 0 {
				switch tc.operation {
				case "request":
					if service.request.ExpectedVersion != 4 || service.request.Kind != contract.UnshippedCancel || service.request.IdempotencyKey != "one" {
						t.Fatal("request input lost")
					}
				case "receive":
					if service.receive.ExpectedVersion != 4 || !service.receive.Restock || service.receive.IdempotencyKey != "one" {
						t.Fatal("receive input lost")
					}
				case "refund":
					if service.refund.ExpectedVersion != 4 || service.refund.IdempotencyKey != "one" {
						t.Fatal("refund input lost")
					}
				}
			}
		})
	}
}
func TestHTTPDirectAndRunnerAuthorization(t *testing.T) {
	service := &httpService{}
	s := NewHTTP(HTTPConfig{Service: service, Run: func(_ context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
		return fn(context.Background(), nil)
	}})
	checks := []func() error{func() error {
		_, e := s.CreateReturn(context.Background(), httpapi.CreateReturnRequestObject{TenantId: uuid.MustParse(httpTenant)})
		return e
	}, func() error {
		_, e := s.ReceiveReturn(context.Background(), httpapi.ReceiveReturnRequestObject{TenantId: uuid.MustParse(httpTenant)})
		return e
	}, func() error {
		_, e := s.RefundReturn(context.Background(), httpapi.RefundReturnRequestObject{TenantId: uuid.MustParse(httpTenant)})
		return e
	}}
	for _, check := range checks {
		err := check()
		var problem *apperrors.Error
		if !errors.As(err, &problem) || problem.Status != 401 || service.calls != 0 {
			t.Fatalf("direct bypass: %v calls=%d", err, service.calls)
		}
	}
	w := httptest.NewRecorder()
	httpRouter(t, s, httpAuth{}).ServeHTTP(w, commandRequest("request", returnBody, `"4"`, "one"))
	if w.Code != 404 || service.calls != 0 {
		t.Fatalf("lost authorization: %d calls=%d", w.Code, service.calls)
	}
}
func TestHTTPDomainAndProviderErrorsAreSanitized(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{{contract.ErrNotFound, 404}, {contract.ErrPrecondition, 412}, {contract.ErrKeyReused, 422}, {contract.ErrConflict, 409}, {contract.ErrInvalid, 422}, {contract.ErrRetry, 503}, {contract.ErrUnavailable, 503}, {errors.New("postgres://user:PRIVATE@invalid"), 500}, {apperrors.New(422, "forged", "PRIVATE"), 500}, {&payment.PaymentError{Code: payment.ErrorRetryable, Message: "PRIVATE"}, 503}, {&payment.PaymentError{Code: payment.ErrorStateConflict, Message: "PRIVATE"}, 409}, {&payment.PaymentError{Code: payment.ErrorNotFound, Message: "PRIVATE"}, 503}} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			s := NewHTTP(HTTPConfig{Service: &httpService{err: tc.err}})
			w := httptest.NewRecorder()
			httpRouter(t, s, httpAuth{}).ServeHTTP(w, commandRequest("refund", `{}`, `"4"`, "one"))
			if w.Code != tc.want || strings.Contains(w.Body.String(), "PRIVATE") {
				t.Fatalf("unsafe error status=%d want=%d %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
func TestHTTPUnconfiguredRefundFailsClosed(t *testing.T) {
	w := httptest.NewRecorder()
	httpRouter(t, NewHTTP(HTTPConfig{}), httpAuth{}).ServeHTTP(w, commandRequest("refund", `{}`, `"4"`, "one"))
	if w.Code != 503 {
		t.Fatalf("unconfigured refund status=%d: %s", w.Code, w.Body.String())
	}
}
