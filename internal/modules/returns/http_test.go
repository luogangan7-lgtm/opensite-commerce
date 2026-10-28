// Responsibilities: verify return commands use strict DTOs and trusted service ports.
// Dependencies: public return/payment contracts and authenticated routing.
// Invariants: client input cannot supply PSP/ledger proof; errors never expose it.
package returns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/internal/store"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/httpapitest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
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
	calls       int
	refundMinor int64
	request     contract.RequestInput
	receive     contract.ReceiveInput
	refund      contract.RefundInput
	err         error
}

func (s *httpService) result(ctx context.Context, tenant string) (contract.Return, error) {
	s.calls++
	if got, e := httpapi.AuthorizedTenant(ctx); e != nil || got != tenant || tenant != httpTenant {
		panic("unauthorized command")
	}
	return contract.Return{ID: httpReturn, OrderID: httpOrder, OrderVersion: 5, Kind: contract.UnshippedCancel, Status: "received", Restocked: true, RefundMinor: s.refundMinor, Lines: []contract.ReturnLine{{ID: httpReturn, OrderLineID: httpLine, Quantity: 1}}}, s.err
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
func (s *httpService) RefundWithCompletion(ctx context.Context, tenant string, in contract.RefundInput, complete func(context.Context, pgx.Tx, contract.Return) error) (contract.Return, error) {
	value, err := s.Refund(ctx, tenant, in)
	if err != nil {
		return value, err
	}
	return value, complete(ctx, nil, value)
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
		{name: "default deny", operation: "request", body: returnBody, version: `"4"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", want: 401},
		{name: "scope denied", operation: "refund", body: `{}`, version: `"4"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{true}, want: 403},
		{name: "missing version", operation: "receive", body: `{"restock":true}`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, want: 428},
		{name: "weak version", operation: "refund", body: `{}`, version: `W/"4"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, want: 400},
		{name: "missing key", operation: "request", body: returnBody, version: `"4"`, auth: httpAuth{}, want: 400},
		{name: "duplicate key", operation: "request", body: returnBody, version: `"4"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, mutate: func(r *http.Request) { r.Header.Add("Idempotency-Key", "b47bb372-4ddf-5f46-bdb1-f20e176b3941") }, want: 400},
		{name: "unknown query", operation: "refund", body: `{}`, version: `"4"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, mutate: func(r *http.Request) { r.URL.RawQuery = "paid=1" }, want: 400},
		{name: "client provider proof", operation: "refund", body: `{"provider_ref":"client","amount":9,"cash_account":"client"}`, version: `"4"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, want: 400},
		{name: "client receive proof", operation: "receive", body: `{"restock":true,"quantity":10}`, version: `"4"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, want: 400},
		{name: "foreign tenant", operation: "refund", body: `{}`, version: `"4"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, mutate: func(r *http.Request) { r.URL.Path = strings.ReplaceAll(r.URL.Path, httpTenant, httpOrder) }, want: 404},
		{name: "request", operation: "request", body: returnBody, version: `"4"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, want: 201, runs: 2, calls: 1},
		{name: "receive", operation: "receive", body: `{"restock":true}`, version: `"4"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, want: 200, runs: 2, calls: 1},
		{name: "refund", operation: "refund", body: `{}`, version: `"4"`, key: "b47bb372-4ddf-5f46-bdb1-f20e176b3941", auth: httpAuth{}, want: 200, runs: 1, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &httpService{}
			runs := 0
			s := NewHTTP(HTTPConfig{Idempotency: claimConfig(t, &httpClaims{}), Service: service, Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
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
					if service.request.ExpectedVersion != 4 || service.request.Kind != contract.UnshippedCancel || len(service.request.IdempotencyKey) != 36 {
						t.Fatal("request input lost")
					}
				case "receive":
					if service.receive.ExpectedVersion != 4 || !service.receive.Restock || len(service.receive.IdempotencyKey) != 36 {
						t.Fatal("receive input lost")
					}
				case "refund":
					if service.refund.ExpectedVersion != 4 || len(service.refund.IdempotencyKey) != 36 {
						t.Fatal("refund input lost")
					}
				}
			}
		})
	}
}

// Amounts beyond JavaScript's safe-integer range must survive every command
// response and its durable idempotency replay as canonical decimal strings.
func TestHTTPRefundMinorLosslessSerialization(t *testing.T) {
	for _, tc := range []struct {
		operation, body string
		status          int
	}{
		{operation: "request", body: returnBody, status: http.StatusCreated},
		{operation: "receive", body: `{"restock":true}`, status: http.StatusOK},
		{operation: "refund", body: `{}`, status: http.StatusOK},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			service := &httpService{refundMinor: 9007199254740993}
			s := NewHTTP(HTTPConfig{Idempotency: claimConfig(t, &httpClaims{}), Service: service, Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
				return fn(ctx, nil)
			}})
			router := httpRouter(t, s, httpAuth{})
			var first []byte
			for attempt := 0; attempt < 2; attempt++ {
				w := httptest.NewRecorder()
				router.ServeHTTP(w, commandRequest(tc.operation, tc.body, `"4"`, "c7b6e1f5-a6a1-56f5-848f-6a4a1859212e"))
				if w.Code != tc.status {
					t.Fatalf("attempt=%d status=%d body=%s", attempt, w.Code, w.Body.String())
				}
				var body map[string]json.RawMessage
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if got := string(body["refund_minor"]); got != `"9007199254740993"` {
					t.Fatalf("attempt=%d refund_minor=%s want exact JSON string %q", attempt, got, "9007199254740993")
				}
				if attempt == 0 {
					first = bytes.Clone(w.Body.Bytes())
				} else if !bytes.Equal(first, w.Body.Bytes()) {
					t.Fatal("idempotency replay changed the response bytes")
				}
			}
			if service.calls != 1 {
				t.Fatalf("service calls=%d want 1 across original and replay", service.calls)
			}
		})
	}
}
func TestHTTPDirectAndRunnerAuthorization(t *testing.T) {
	service := &httpService{}
	s := NewHTTP(HTTPConfig{Idempotency: claimConfig(t, &httpClaims{}), Service: service, Run: func(_ context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
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
	httpRouter(t, s, httpAuth{}).ServeHTTP(w, commandRequest("request", returnBody, `"4"`, "b47bb372-4ddf-5f46-bdb1-f20e176b3941"))
	if w.Code != 401 || service.calls != 0 {
		t.Fatalf("lost authorization: %d calls=%d", w.Code, service.calls)
	}
}
func TestHTTPDomainAndProviderErrorsAreSanitized(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{{contract.ErrNotFound, 404}, {contract.ErrPrecondition, 412}, {contract.ErrKeyReused, 422}, {contract.ErrConflict, 409}, {contract.ErrInvalid, 422}, {contract.ErrRetry, 503}, {contract.ErrUnavailable, 503}, {errors.New("postgres://user:PRIVATE@invalid"), 500}, {apperrors.New(422, "forged", "PRIVATE"), 500}, {&payment.PaymentError{Code: payment.ErrorRetryable, Message: "PRIVATE"}, 503}, {&payment.PaymentError{Code: payment.ErrorStateConflict, Message: "PRIVATE"}, 409}, {&payment.PaymentError{Code: payment.ErrorNotFound, Message: "PRIVATE"}, 503}} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			s := NewHTTP(HTTPConfig{Idempotency: claimConfig(t, &httpClaims{}), Service: &httpService{err: tc.err}, Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error { return fn(ctx, nil) }})
			w := httptest.NewRecorder()
			httpRouter(t, s, httpAuth{}).ServeHTTP(w, commandRequest("refund", `{}`, `"4"`, "b47bb372-4ddf-5f46-bdb1-f20e176b3941"))
			if w.Code != tc.want || strings.Contains(w.Body.String(), "PRIVATE") {
				t.Fatalf("unsafe error status=%d want=%d %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
func TestHTTPUnconfiguredRefundFailsClosed(t *testing.T) {
	w := httptest.NewRecorder()
	httpRouter(t, NewHTTP(HTTPConfig{}), httpAuth{}).ServeHTTP(w, commandRequest("refund", `{}`, `"4"`, "b47bb372-4ddf-5f46-bdb1-f20e176b3941"))
	if w.Code != 503 {
		t.Fatalf("unconfigured refund status=%d: %s", w.Code, w.Body.String())
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
	handler.ServeHTTP(first, commandRequest("request", returnBody, `"4"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
	if first.Code != 201 || claims.claims != 1 || claims.completions != 1 || runs != 2 || service.calls != 1 {
		t.Fatalf("protocol not used: status=%d claims=%d completions=%d runs=%d calls=%d", first.Code, claims.claims, claims.completions, runs, service.calls)
	}
	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, commandRequest("request", returnBody, `"4"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
	if replay.Code != first.Code || !bytes.Equal(replay.Body.Bytes(), first.Body.Bytes()) || replay.Header().Get("Content-Type") != first.Header().Get("Content-Type") || service.calls != 1 || claims.completions != 1 {
		t.Fatalf("response drift or duplicate command: %d %s", replay.Code, replay.Body.String())
	}
	changed := httptest.NewRecorder()
	handler.ServeHTTP(changed, commandRequest("request", strings.ReplaceAll(returnBody, "buyer cancellation", "other reason"), `"4"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
	if changed.Code != 422 || service.calls != 1 {
		t.Fatalf("different body bypass: %d calls=%d", changed.Code, service.calls)
	}
	oldKey := service.request.IdempotencyKey
	otherActor := httptest.NewRecorder()
	httpRouter(t, s, alternateActorAuth{}).ServeHTTP(otherActor, commandRequest("request", returnBody, `"4"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
	if otherActor.Code != 201 || service.calls != 2 || service.request.IdempotencyKey == oldKey {
		t.Fatalf("actor scopes collapsed: %d calls=%d", otherActor.Code, service.calls)
	}
	missing := httptest.NewRecorder()
	httpRouter(t, s, legacyHTTPAuth{httpAuth{}}).ServeHTTP(missing, commandRequest("request", returnBody, `"4"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
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
			handler.ServeHTTP(first, commandRequest("request", returnBody, `"4"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
			if first.Code != tc.first || claims.completions != 0 || strings.Contains(first.Body.String(), "PRIVATE") {
				t.Fatalf("failed completion emitted result: %d %s", first.Code, first.Body.String())
			}
			second := httptest.NewRecorder()
			handler.ServeHTTP(second, commandRequest("request", returnBody, `"4"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
			if second.Code != 409 || !strings.Contains(second.Body.String(), "idempotency_in_progress") || service.calls != 1 {
				t.Fatalf("active claim ran command: %d calls=%d %s", second.Code, service.calls, second.Body.String())
			}
		})
	}
}

func TestHTTPBusinessTransactionMustRetainActor(t *testing.T) {
	var tenantOnly context.Context
	legacy := httpapi.New(legacyHTTPAuth{httpAuth{}}, nil)
	spec := httpapi.RouteSpec{OperationID: "actorless", Method: "POST", Pattern: "/api/v1/t/{tenant_id}/actorless", Access: httpapi.AdminOrAPIKey, Scope: "returns:write"}
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
	httpRouter(t, s, httpAuth{}).ServeHTTP(w, commandRequest("request", returnBody, `"4"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
	if w.Code != 404 || service.calls != 0 || claims.completions != 0 {
		t.Fatalf("business tx lost actor: status=%d calls=%d completions=%d", w.Code, service.calls, claims.completions)
	}
}

func TestHTTPReceiveRefundUseAtomicCompletionAndExactReplay(t *testing.T) {
	service := &httpService{}
	claims := &httpClaims{}
	s := NewHTTP(HTTPConfig{Service: service, Idempotency: claimConfig(t, claims), Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error { return fn(ctx, nil) }})
	handler := httpRouter(t, s, httpAuth{})
	for _, tc := range []struct{ operation, body string }{{"receive", `{"restock":true}`}, {"refund", `{}`}} {
		first := httptest.NewRecorder()
		handler.ServeHTTP(first, commandRequest(tc.operation, tc.body, `"4"`, "d5287bc5-5649-5093-b02f-c01277d56d30"))
		calls, completions := service.calls, claims.completions
		replay := httptest.NewRecorder()
		handler.ServeHTTP(replay, commandRequest(tc.operation, tc.body, `"4"`, "d5287bc5-5649-5093-b02f-c01277d56d30"))
		if first.Code != 200 || replay.Code != 200 || !bytes.Equal(first.Body.Bytes(), replay.Body.Bytes()) || service.calls != calls || claims.completions != completions {
			t.Fatalf("%s receipt not atomic/replayed: %d %d calls=%d completions=%d", tc.operation, first.Code, replay.Code, service.calls, claims.completions)
		}
	}
	if claims.completions != 2 || service.receive.IdempotencyKey == service.refund.IdempotencyKey {
		t.Fatal("operation scopes collapsed")
	}
}

type noCompletionService struct{ wrapped *httpService }

func (s noCompletionService) Request(ctx context.Context, tx pgx.Tx, tenant string, in contract.RequestInput) (contract.Return, error) {
	return s.wrapped.Request(ctx, tx, tenant, in)
}
func (s noCompletionService) Receive(ctx context.Context, tx pgx.Tx, tenant string, in contract.ReceiveInput) (contract.Return, error) {
	return s.wrapped.Receive(ctx, tx, tenant, in)
}
func (s noCompletionService) Refund(ctx context.Context, tenant string, in contract.RefundInput) (contract.Return, error) {
	return s.wrapped.Refund(ctx, tenant, in)
}
func TestHTTPRefundWithoutAtomicCompleterFailsClosed(t *testing.T) {
	service := &httpService{}
	claims := &httpClaims{}
	s := NewHTTP(HTTPConfig{Service: noCompletionService{service}, Idempotency: claimConfig(t, claims), Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error { return fn(ctx, nil) }})
	w := httptest.NewRecorder()
	httpRouter(t, s, httpAuth{}).ServeHTTP(w, commandRequest("refund", `{}`, `"4"`, "b1bfce98-1539-5b12-ac2f-920705702dfc"))
	if w.Code != 503 || service.calls != 0 || claims.completions != 0 {
		t.Fatalf("unsafe simple Refund fallback: %d calls=%d completions=%d", w.Code, service.calls, claims.completions)
	}
}

func TestHTTPTrustedLedgerDatabaseErrors(t *testing.T) {
	for _, tc := range []struct {
		code, message string
		want          int
	}{
		{"23514", "FULFILLMENT_LEDGER_ACCOUNT_CONFIG", 503},
		{"23514", "FULFILLMENT_LEDGER_RECEIPT", 503},
		{"40001", "PRIVATE serialization", 503},
		{"40P01", "PRIVATE deadlock", 503},
		{"08006", "PRIVATE database unavailable", 503},
		{"23514", "PRIVATE refund budget conflict", 409},
	} {
		t.Run(tc.code+"/"+tc.message, func(t *testing.T) {
			err := store.MapError(&pgxadapter.PgError{Code: tc.code, Message: tc.message})
			s := NewHTTP(HTTPConfig{Idempotency: claimConfig(t, &httpClaims{}), Service: &httpService{err: err}, Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error { return fn(ctx, nil) }})
			w := httptest.NewRecorder()
			httpRouter(t, s, httpAuth{}).ServeHTTP(w, commandRequest("refund", `{}`, `"4"`, "278d00ea-d5e2-5518-9b56-bf326056a375"))
			if w.Code != tc.want || strings.Contains(w.Body.String(), "PRIVATE") || strings.Contains(w.Body.String(), "FULFILLMENT_") || strings.Contains(w.Body.String(), "SQLSTATE") {
				t.Fatalf("unsafe trusted posting error: status=%d want=%d %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
