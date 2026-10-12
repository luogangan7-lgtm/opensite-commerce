// Responsibilities: exercise quote transport trust, strict admission and headers.
// Dependencies: quotes contract and httptest; callers: focused tests and CI.
// Invariants: rejected requests never reach the service; no SQL proof is implied.
package quotes_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	quoteshttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/quotes"
)

const quoteTenant = "11111111-1111-1111-1111-111111111111"
const quoteID = "22222222-2222-2222-2222-222222222222"
const quoteActor = "33333333-3333-3333-3333-333333333333"
const quoteCompany = "44444444-4444-4444-4444-444444444444"

type quoteFixture struct {
	view        *qc.Quote
	createInput qc.CreateInput
	calls       int
	input       qc.InquiryInput
	opts        qc.WriteOptions
	fail        error
}

func (f *quoteFixture) SubmitInquiry(_ context.Context, in qc.InquiryInput) (qc.Inquiry, error) {
	f.calls++
	f.input = in
	return qc.Inquiry{ID: quoteID, Status: "received"}, f.fail
}
func (f *quoteFixture) Create(_ context.Context, in qc.CreateInput, o qc.WriteOptions) (qc.Quote, error) {
	f.calls++
	f.opts = o
	f.createInput = in
	return quoteView(), f.fail
}
func (f *quoteFixture) Get(context.Context, string) (qc.Quote, error) {
	f.calls++
	if f.view != nil {
		return *f.view, f.fail
	}
	return quoteView(), f.fail
}
func (f *quoteFixture) Transition(_ context.Context, _ string, _ qc.Action, o qc.WriteOptions) (qc.Quote, error) {
	f.calls++
	f.opts = o
	return quoteView(), f.fail
}
func quoteView() qc.Quote {
	return qc.Quote{ID: quoteID, TenantID: quoteTenant, SiteID: quoteTenant, InquiryID: quoteID, CompanyID: quoteCompany, CreatedBy: quoteActor, Status: "draft", Version: 1, Currency: "USD", ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), Snapshot: qc.Snapshot{ShippingProfileID: quoteID, Lines: []qc.Line{}, Currency: "USD"}}
}

type quoteAuth struct{ kind string }

func (a quoteAuth) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	i := qc.ActorIdentity{TenantID: quoteTenant}
	if a.kind == "staff" {
		i.StaffID = quoteActor
	} else if a.kind == "customer" {
		i.CustomerID = quoteActor
		i.CompanyID = quoteCompany
	} else {
		return ctx, nil
	}
	return qc.WithTrustedActor(ctx, i), nil
}
func quoteRouter(t *testing.T, f *quoteFixture, staff, buyer httpapi.Authenticator, inquiry quoteshttp.QuotesInquiryResolver) http.Handler {
	t.Helper()
	r := httpapi.New(staff, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, v := range quoteshttp.NewQuotesHandler(f, buyer, inquiry).Routes() {
		if err := r.Register(v.Spec, v.Handler); err != nil {
			t.Fatal(err)
		}
	}
	return r
}
func quoteRequest(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "127.0.0.1:9999"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestQuotesTrustedBuyerCannotBeBypassed(t *testing.T) {
	for _, buyer := range []httpapi.Authenticator{nil, quoteAuth{kind: "none"}, quoteAuth{kind: "staff"}} {
		f := &quoteFixture{}
		h := quoteRouter(t, f, quoteAuth{kind: "staff"}, buyer, nil)
		for _, suffix := range []string{"", "/accept", "/approve", "/reject"} {
			method, body := "POST", "{}"
			if suffix == "" {
				method, body = "GET", ""
			}
			w := quoteRequest(h, method, "/api/public/v1/quotes/"+quoteID+suffix, body, map[string]string{"If-Match": quoteView().ETag(), "Idempotency-Key": "k", "X-Customer-ID": quoteActor, "X-Company-ID": quoteCompany, "Authorization": "Bearer fake"})
			if w.Code != 401 {
				t.Errorf("suffix=%s status=%d body=%s", suffix, w.Code, w.Body.String())
			}
		}
		if f.calls != 0 {
			t.Fatalf("untrusted actor reached service %d times", f.calls)
		}
	}
}
func TestQuotesStaffAuthenticationAndTenantRequired(t *testing.T) {
	for _, staff := range []httpapi.Authenticator{nil, quoteAuth{kind: "none"}, quoteAuth{kind: "customer"}} {
		f := &quoteFixture{}
		h := quoteRouter(t, f, staff, nil, nil)
		w := quoteRequest(h, "GET", "/api/v1/t/"+quoteTenant+"/quotes/"+quoteID, "", nil)
		if w.Code != 401 || f.calls != 0 {
			t.Fatalf("status=%d calls=%d", w.Code, f.calls)
		}
	}
	f := &quoteFixture{}
	h := quoteRouter(t, f, quoteAuth{kind: "staff"}, nil, nil)
	w := quoteRequest(h, "GET", "/api/v1/t/"+quoteCompany+"/quotes/"+quoteID, "", nil)
	if w.Code != 404 || f.calls != 0 {
		t.Fatalf("tenant mismatch %d %d", w.Code, f.calls)
	}
}
func TestQuotesStrictAdmissionBeforeService(t *testing.T) {
	f := &quoteFixture{}
	h := quoteRouter(t, f, quoteAuth{kind: "staff"}, quoteAuth{kind: "customer"}, nil)
	headers := map[string]string{"If-Match": quoteView().ETag(), "Idempotency-Key": "k"}
	for _, body := range []string{`{"company_id":"` + quoteCompany + `"}`, `{"x":1,"x":2}`, `{} {}`, `null`, `[]`} {
		w := quoteRequest(h, "POST", "/api/public/v1/quotes/"+quoteID+"/accept", body, headers)
		if w.Code != 400 {
			t.Errorf("body=%s status=%d", body, w.Code)
		}
	}
	if f.calls != 0 {
		t.Fatalf("bad body reached service %d", f.calls)
	}
}
func TestQuotesWritePreconditions(t *testing.T) {
	for _, tc := range []struct {
		match, key string
		status     int
	}{{"", "k", 428}, {"*", "k", 412}, {"W/" + quoteView().ETag(), "k", 412}, {quoteView().ETag(), "", 400}, {quoteView().ETag(), "k", 200}} {
		f := &quoteFixture{}
		h := quoteRouter(t, f, quoteAuth{kind: "staff"}, nil, nil)
		w := quoteRequest(h, "POST", "/api/v1/t/"+quoteTenant+"/quotes/"+quoteID+"/send", "{}", map[string]string{"If-Match": tc.match, "Idempotency-Key": tc.key})
		if w.Code != tc.status {
			t.Errorf("match=%q key=%q status=%d body=%s", tc.match, tc.key, w.Code, w.Body.String())
		}
		if tc.status == 200 && (w.Header().Get("ETag") != quoteView().ETag() || f.opts.IfMatch != tc.match) {
			t.Fatal("version lost")
		}
		if tc.status != 200 && f.calls != 0 {
			t.Fatal("invalid precondition reached service")
		}
	}
}
func TestQuotesInquiryVerifiedHostAndRateLimit(t *testing.T) {
	f := &quoteFixture{}
	h := quoteRouter(t, f, nil, nil, nil)
	body := `{"buyer_email":"buyer@example.test","message":"hello"}`
	w := quoteRequest(h, "POST", "/api/public/v1/inquiries", body, nil)
	if w.Code != 401 || f.calls != 0 {
		t.Fatalf("missing host resolver %d/%d", w.Code, f.calls)
	}
	resolver := func(ctx context.Context, _ *http.Request) (context.Context, error) {
		return qc.WithTrustedInquiry(ctx, quoteTenant, quoteID), nil
	}
	h = quoteRouter(t, f, nil, nil, resolver)
	for i := 0; i < 6; i++ {
		w = quoteRequest(h, "POST", "/api/public/v1/inquiries", body, map[string]string{"X-Forwarded-For": "forged"})
		want := 202
		if i == 5 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("request=%d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	if f.calls != 5 || f.input.SiteID != quoteID || f.input.Email != "buyer@example.test" {
		t.Fatalf("inquiry calls=%d input=%+v", f.calls, f.input)
	}
}

func quoteCreateBody() string {
	return `{"site_id":"` + quoteTenant + `","inquiry_id":"` + quoteID + `","company_id":"` + quoteCompany + `","currency":"USD","expires_at":"2030-01-01T00:00:00Z","lines":[{"product_id":"` + quoteID + `","description":"widget","quantity":2,"unit_price":9007199254740993,"tax_category":"standard"}],"destination":{"country":"US","subdivision":"CA"},"shipping_profile_id":"` + quoteID + `","weight_g":12}`
}
func TestQuotesCreateUsesGeneratedStrictAdapter(t *testing.T) {
	f := &quoteFixture{}
	h := quoteRouter(t, f, quoteAuth{kind: "staff"}, nil, nil)
	w := quoteRequest(h, "POST", "/api/v1/t/"+quoteTenant+"/quotes", quoteCreateBody(), map[string]string{"If-Match": "*", "Idempotency-Key": "create-one"})
	if w.Code != 201 || f.calls != 1 || f.createInput.Lines[0].UnitPrice != 9007199254740993 || f.opts.IfMatch != "*" {
		t.Fatalf("status=%d calls=%d input=%+v body=%s", w.Code, f.calls, f.createInput, w.Body.String())
	}
	var body map[string]any
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || body["id"] != quoteID || body["version"] != float64(1) || w.Header().Get("ETag") != quoteView().ETag() {
		t.Fatalf("representation lost %v", body)
	}
	for _, bad := range []string{strings.Replace(quoteCreateBody(), `"unit_price":9007199254740993`, `"unit_price":9223372036854775808`, 1), strings.Replace(quoteCreateBody(), `"currency":"USD"`, `"Currency":"USD"`, 1), strings.Replace(quoteCreateBody(), `"tax_category":"standard"`, `"tax_category":"standard","company_id":"`+quoteCompany+`"`, 1), strings.Replace(quoteCreateBody(), `"weight_g":12`, `"weight_g":12,"weight_g":13`, 1), strings.Replace(quoteCreateBody(), `"quantity":2`, `"quantity":0`, 1)} {
		w = quoteRequest(h, "POST", "/api/v1/t/"+quoteTenant+"/quotes", bad, map[string]string{"If-Match": "*", "Idempotency-Key": "bad-create"})
		if w.Code != 400 {
			t.Errorf("invalid create admitted status=%d body=%s", w.Code, w.Body.String())
		}
	}
	if f.calls != 1 {
		t.Fatalf("bad create reached service %d", f.calls)
	}
}
func TestQuotesSnapshotMinorUnitsRemainExact(t *testing.T) {
	for _, tc := range []struct{ amount, shipping int64 }{{9007199254740993, 9007199254740993}, {9223372036854775807, 0}} {
		q := quoteView()
		q.Snapshot.Subtotal, q.Snapshot.Total = tc.amount, tc.amount+tc.shipping
		q.Snapshot.ShippingAmount = tc.shipping
		q.Snapshot.Shipping.Charge.Amount, q.Snapshot.Shipping.Charge.Currency = tc.shipping, "USD"
		q.Snapshot.Tax.Currency = "USD"
		q.Snapshot.Tax.Totals.Net, q.Snapshot.Tax.Totals.Gross = tc.amount, tc.amount
		line := qc.Line{LineNo: 1, Description: "zero-rated", Quantity: 1, UnitPrice: tc.amount, LineTotal: tc.amount}
		line.Snapshot.Amounts.Net, line.Snapshot.Amounts.Gross = tc.amount, tc.amount
		q.Snapshot.Lines = []qc.Line{line}
		q.Snapshot.Tax.Lines = append(q.Snapshot.Tax.Lines, line.Snapshot)
		f := &quoteFixture{view: &q}
		w := quoteRequest(quoteRouter(t, f, quoteAuth{kind: "staff"}, nil, nil), "GET", "/api/v1/t/"+quoteTenant+"/quotes/"+quoteID, "", nil)
		var body struct {
			Snapshot struct {
				Tax      struct{ Totals struct{ Net int64 } }
				Shipping struct{ Charge struct{ Amount int64 } }
				Lines    []struct {
					Snapshot struct{ Amounts struct{ Net int64 } }
				}
			}
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Snapshot.Tax.Totals.Net != tc.amount || body.Snapshot.Shipping.Charge.Amount != tc.shipping || len(body.Snapshot.Lines) != 1 || body.Snapshot.Lines[0].Snapshot.Amounts.Net != tc.amount {
			t.Fatalf("minor units changed: amount=%d shipping=%d HTTP%d %s", tc.amount, tc.shipping, w.Code, w.Body.String())
		}
	}
}
func TestQuotesErrorsAreStableAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{{qc.ErrUnauthorized, 401, "unauthenticated"}, {qc.ErrForbidden, 403, "forbidden"}, {qc.ErrNotFound, 404, "not_found"}, {qc.ErrInvalid, 422, "validation_failed"}, {qc.ErrOverflow, 422, "amount_overflow"}, {qc.ErrPreconditionRequired, 428, "precondition_required"}, {qc.ErrPrecondition, 412, "etag_mismatch"}, {qc.ErrConflict, 409, "quote_conflict"}, {qc.ErrIdempotencyReused, 422, "idempotency_key_reused"}, {qc.ErrUnavailable, 500, "internal_error"}} {
		f := &quoteFixture{fail: tc.err}
		h := quoteRouter(t, f, quoteAuth{kind: "staff"}, nil, nil)
		w := quoteRequest(h, "GET", "/api/v1/t/"+quoteTenant+"/quotes/"+quoteID, "", nil)
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != tc.status || body["code"] != tc.code || w.Header().Get("Content-Type") != "application/problem+json" || strings.Contains(w.Body.String(), tc.err.Error()) {
			t.Fatalf("err=%v status=%d body=%s", tc.err, w.Code, w.Body.String())
		}
	}
}
func TestQuotesEveryLifecycleRouteReachesService(t *testing.T) {
	f := &quoteFixture{}
	h := quoteRouter(t, f, quoteAuth{kind: "staff"}, quoteAuth{kind: "customer"}, nil)
	for _, path := range []string{"/api/v1/t/" + quoteTenant + "/quotes/" + quoteID, "/api/public/v1/quotes/" + quoteID} {
		w := quoteRequest(h, "GET", path, "", nil)
		if w.Code != 200 {
			t.Fatalf("%s=%d %s", path, w.Code, w.Body.String())
		}
	}
	for _, action := range []string{"send", "withdraw", "expire", "accept", "approve", "reject"} {
		path := "/api/v1/t/" + quoteTenant + "/quotes/" + quoteID + "/" + action
		if action == "accept" || action == "approve" || action == "reject" {
			path = "/api/public/v1/quotes/" + quoteID + "/" + action
		}
		w := quoteRequest(h, "POST", path, "{}", map[string]string{"If-Match": quoteView().ETag(), "Idempotency-Key": action})
		if w.Code != 200 {
			t.Errorf("%s=%d %s", path, w.Code, w.Body.String())
		}
	}
	if f.calls != 8 {
		t.Fatalf("calls=%d", f.calls)
	}
}
func TestQuotesDuplicateHeaderAndBodyLimit(t *testing.T) {
	f := &quoteFixture{}
	h := quoteRouter(t, f, quoteAuth{kind: "staff"}, nil, nil)
	for _, name := range []string{"If-Match", "Idempotency-Key"} {
		r := httptest.NewRequest("POST", "/api/v1/t/"+quoteTenant+"/quotes/"+quoteID+"/send", strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("If-Match", quoteView().ETag())
		r.Header.Set("Idempotency-Key", "k")
		r.Header.Add(name, r.Header.Get(name))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 400
		if name == "If-Match" {
			want = 412
		}
		if w.Code != want {
			t.Fatalf("duplicate %s=%d", name, w.Code)
		}
	}
	w := quoteRequest(h, "POST", "/api/v1/t/"+quoteTenant+"/quotes/"+quoteID+"/send", `{"x":"`+strings.Repeat("x", 1024)+`"}`, map[string]string{"If-Match": quoteView().ETag(), "Idempotency-Key": "large"})
	if w.Code != 413 || f.calls != 0 {
		t.Fatalf("oversize %d %d", w.Code, f.calls)
	}
	r := httptest.NewRequest("POST", "/api/v1/t/"+quoteTenant+"/quotes/"+quoteID+"/send", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("If-Match", quoteView().ETag())
	r.Header.Set("Idempotency-Key", "wrongtype")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 415 || f.calls != 0 {
		t.Fatalf("contenttype %d %d", w.Code, f.calls)
	}
}
