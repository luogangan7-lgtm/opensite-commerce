// Platform cart transport tests execute mounted generated wrappers with trusted actor
// fixtures and an independent service port. No database or staff auth proof.
// The gate rejects malformed JSON/preconditions before any service mutation.
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
)

const tenant = "11111111-1111-4111-8111-111111111111"
const cartID = "22222222-2222-4222-8222-222222222222"
const sku = "33333333-3333-4333-8333-333333333333"
const priceList = "44444444-4444-4444-8444-444444444444"
const token = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
const base = "/api/v1/t/" + tenant + "/carts"

type fakeService struct {
	view    contract.View
	calls   int
	owner   string
	failure error
}

func (s *fakeService) Create(_ context.Context, _ contract.AuthorizedActor, _ string) (contract.Created, error) {
	s.calls++
	return contract.Created{Cart: s.view, CartToken: token}, s.failure
}
func (s *fakeService) Get(_ context.Context, a contract.AuthorizedActor, _ string) (contract.View, error) {
	s.calls++
	if a.CustomerID != s.owner {
		return contract.View{}, apperrors.New(404, "not_found", "Cart not found")
	}
	return s.view, s.failure
}
func (s *fakeService) PutLine(ctx context.Context, a contract.AuthorizedActor, id, _ string, _ int64, tag string) (contract.View, error) {
	if tag != contract.ETag(s.view) {
		return contract.View{}, apperrors.New(412, "precondition_failed", "Cart changed")
	}
	return s.Get(ctx, a, id)
}
func (s *fakeService) DeleteLine(ctx context.Context, a contract.AuthorizedActor, id, _ string, tag string) (contract.View, error) {
	return s.PutLine(ctx, a, id, "", 0, tag)
}

type authFunc func(context.Context, *http.Request, RouteSpec) (context.Context, error)

func (f authFunc) Authenticate(ctx context.Context, r *http.Request, s RouteSpec) (context.Context, error) {
	return f(ctx, r, s)
}
func view() contract.View {
	return contract.View{ID: cartID, Currency: "USD", Version: 1, Subtotal: 200, Lines: []contract.Line{{SKUID: sku, Quantity: 2, UnitAmount: 100, LineAmount: 200, PriceListID: priceList, MinQuantity: 1, AvailableQuantity: 5, Sellable: true}}}
}
func router(t *testing.T, s *fakeService, a contract.AuthorizedActor) *Router {
	t.Helper()
	r := New(authFunc(func(ctx context.Context, _ *http.Request, _ RouteSpec) (context.Context, error) {
		return contract.WithActor(ctx, a), nil
	}), nil)
	for _, route := range NewCartHandler(s).Routes() {
		if err := r.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	return r
}
func request(r *Router, method, path, body, tag string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tag != "" {
		req.Header.Set("If-Match", tag)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func TestStrictCartAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body, tag string
		status                        int
	}{
		{"missing precondition", "PUT", base + "/" + cartID + "/lines/" + sku, `{"quantity":2}`, "", 428},
		{"delete missing precondition", "DELETE", base + "/" + cartID + "/lines/" + sku, "", "", 428},
		{"unknown field", "POST", base, `{"currency":"USD","customer_id":"spoof"}`, "", 400},
		{"duplicate escaped key", "POST", base, `{"currency":"USD","\u0063urrency":"EUR"}`, "", 400},
		{"null currency", "POST", base, `{"currency":null}`, "", 400},
		{"missing currency", "POST", base, `{}`, "", 400},
		{"currency wrong case", "POST", base, `{"currency":"usd"}`, "", 400},
		{"null quantity", "PUT", base + "/" + cartID + "/lines/" + sku, `{"quantity":null}`, "tag", 400},
		{"integer overflow", "PUT", base + "/" + cartID + "/lines/" + sku, `{"quantity":9223372036854775808}`, "tag", 400},
		{"fractional quantity", "PUT", base + "/" + cartID + "/lines/" + sku, `{"quantity":1.5}`, "tag", 400},
		{"negative quantity", "PUT", base + "/" + cartID + "/lines/" + sku, `{"quantity":-1}`, "tag", 400},
		{"missing quantity", "PUT", base + "/" + cartID + "/lines/" + sku, `{}`, "tag", 400},
		{"bounded quantity", "PUT", base + "/" + cartID + "/lines/" + sku, `{"quantity":1000001}`, "tag", 400},
		{"trailing body", "POST", base, `{"currency":"USD"}{}`, "", 400},
		{"invalid UUID", "GET", base + "/oops", "", "", 400},
		{"wildcard precondition", "PUT", base + "/" + cartID + "/lines/" + sku, `{"quantity":2}`, "*", 412},
		{"weak precondition", "DELETE", base + "/" + cartID + "/lines/" + sku, "", `W/"v1"`, 412},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeService{view: view(), owner: "owner"}
			w := request(router(t, s, contract.AuthorizedActor{TenantID: tenant, CustomerID: "owner"}), tc.method, tc.path, tc.body, tc.tag)
			if w.Code != tc.status || w.Header().Get("Content-Type") != "application/problem+json" || s.calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, s.calls, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("error cache leaked")
			}
		})
	}
}
func TestOwnerAndTenantCannotLeak(t *testing.T) {
	for _, a := range []contract.AuthorizedActor{{TenantID: tenant, CustomerID: "other"}, {TenantID: "99999999-9999-4999-8999-999999999999", CustomerID: "owner"}, {}} {
		s := &fakeService{view: view(), owner: "owner"}
		w := request(router(t, s, a), "GET", base+"/"+cartID, "", "")
		if w.Code != 404 || strings.Contains(w.Body.String(), sku) || strings.Contains(w.Body.String(), "USD") {
			t.Fatalf("private cart leaked: %s", w.Body.String())
		}
	}
}
func TestCartSuccessUsesGeneratedRepresentations(t *testing.T) {
	for _, method := range []string{"POST", "GET", "PUT", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			s := &fakeService{view: view(), owner: "owner"}
			r := router(t, s, contract.AuthorizedActor{TenantID: tenant, CustomerID: "owner"})
			path, body, status := base+"/"+cartID, "", 200
			if method == "POST" {
				path, body, status = base, `{"currency":"USD"}`, 201
			}
			if method == "PUT" || method == "DELETE" {
				path += "/lines/" + sku
			}
			if method == "PUT" {
				body = `{"quantity":2}`
			}
			w := request(r, method, path, body, contract.ETag(s.view))
			if w.Code != status || w.Header().Get("ETag") != contract.ETag(s.view) || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("response: %d %s", w.Code, w.Body.String())
			}
			var result map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			_, hasToken := result["cart_token"]
			if hasToken != (method == "POST") {
				t.Fatal("creation-only token policy violated")
			}
			if strings.Contains(w.Body.String(), "GuestTokenHash") {
				t.Fatal("hash leaked")
			}
		})
	}
}
func TestETagReflectsLivePriceAndStock(t *testing.T) {
	v := view()
	original := contract.ETag(v)
	v.Lines[0].UnitAmount++
	if contract.ETag(v) == original {
		t.Fatal("price ETag unchanged")
	}
	v = view()
	v.Lines[0].AvailableQuantity--
	if contract.ETag(v) == original {
		t.Fatal("stock ETag unchanged")
	}
	if strings.HasPrefix(original, "W/") {
		t.Fatal("weak ETag")
	}
}
func TestGuestHashNeverAppearsInRead(t *testing.T) {
	hash := sha256.Sum256([]byte(token))
	s := &fakeService{view: view()}
	w := request(router(t, s, contract.AuthorizedActor{TenantID: tenant, GuestTokenHash: hash[:]}), "GET", base+"/"+cartID, "", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), "guest") {
		t.Fatal("guest credentials leaked")
	}
}

func TestSetZeroDeletes(t *testing.T) {
	s := &fakeService{view: view(), owner: "owner"}
	w := request(router(t, s, contract.AuthorizedActor{TenantID: tenant, CustomerID: "owner"}), "PUT", base+"/"+cartID+"/lines/"+sku, `{"quantity":0}`, contract.ETag(s.view))
	if w.Code != 200 || s.calls != 1 {
		t.Fatalf("zero rejected: %d %s", w.Code, w.Body.String())
	}
}

func TestCartTransportLimitsAndDuplicatePreconditions(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body, media string
		tags                            []string
		status                          int
	}{
		{"wrong media", "POST", base, `{"currency":"USD"}`, "text/plain", nil, 415},
		{"bounded wire body", "POST", base, `{"currency":"` + strings.Repeat("A", 4096) + `"}`, "application/json", nil, 413},
		{"duplicate preconditions", "DELETE", base + "/" + cartID + "/lines/" + sku, "", "", []string{"one", "two"}, 412},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeService{view: view(), owner: "owner"}
			r := router(t, s, contract.AuthorizedActor{TenantID: tenant, CustomerID: "owner"})
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.media)
			for _, tag := range tc.tags {
				req.Header.Add("If-Match", tag)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.status || s.calls != 0 || w.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, s.calls, w.Body.String())
			}
		})
	}
}

func TestCartJSONKeysMustBeCanonical(t *testing.T) {
	for _, tc := range []struct{ name, method, body string }{
		{"capital currency", "POST", `{"Currency":"USD"}`},
		{"upper currency", "POST", `{"CURRENCY":"USD"}`},
		{"mixed currency keys", "POST", `{"currency":"USD","Currency":"EUR"}`},
		{"capital quantity", "PUT", `{"Quantity":1}`},
		{"upper quantity", "PUT", `{"QUANTITY":1}`},
		{"mixed quantity deletes", "PUT", `{"quantity":1,"Quantity":0}`},
		{"reverse mixed quantity", "PUT", `{"Quantity":0,"quantity":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeService{view: view(), owner: "owner"}
			r := router(t, s, contract.AuthorizedActor{TenantID: tenant, CustomerID: "owner"})
			path := base
			if tc.method == "PUT" {
				path += "/" + cartID + "/lines/" + sku
			}
			w := request(r, tc.method, path, tc.body, contract.ETag(s.view))
			if w.Code != 400 || s.calls != 0 || w.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("noncanonical keys reached service: status=%d calls=%d body=%s", w.Code, s.calls, w.Body.String())
			}
		})
	}
}
