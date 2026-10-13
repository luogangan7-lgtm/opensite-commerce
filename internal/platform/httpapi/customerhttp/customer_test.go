// Customer transport tests run in GitHub CI, including every cookie and CSRF gate.
// Dependencies: public customer port and httptest; no employee or database authority.
package customerhttp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

const tenant = "11111111-1111-4111-8111-111111111111"
const origin = "https://shop.example.test"
const sessionPath = "/api/v1/t/" + tenant + "/customer/session"

// Domain receipts preserve the exact arguments passed by the public transport.
type domainReceipt struct {
	operation, tenant, email, credential, token, previousToken string
	ip                                                         netip.Addr
}

type fake struct {
	calls        int
	issued       string
	limited      bool
	currentError error
	receipts     []domainReceipt
	reads        []domainReceipt
}

func (f *fake) record(receipt domainReceipt) {
	f.calls++
	f.receipts = append(f.receipts, receipt)
}
func (f *fake) Register(_ context.Context, tenant, email, credential string, ip netip.Addr) error {
	f.record(domainReceipt{operation: "register", tenant: tenant, email: email, credential: credential, ip: ip})
	return nil
}
func (f *fake) Verify(_ context.Context, tenant, token string) error {
	f.record(domainReceipt{operation: "verify", tenant: tenant, token: token})
	return nil
}
func (f *fake) RequestReset(_ context.Context, tenant, email string, ip netip.Addr) error {
	f.record(domainReceipt{operation: "request-reset", tenant: tenant, email: email, ip: ip})
	return nil
}
func (f *fake) Reset(_ context.Context, tenant, token, credential string, ip netip.Addr) error {
	f.record(domainReceipt{operation: "reset", tenant: tenant, token: token, credential: credential, ip: ip})
	f.issued = ""
	return nil
}
func (f *fake) Login(_ context.Context, tenant, email, credential string, ip netip.Addr, previousToken string) (c.Session, error) {
	f.record(domainReceipt{operation: "login", tenant: tenant, email: email, credential: credential, ip: ip, previousToken: previousToken})
	if f.limited {
		return c.Session{}, &c.RateLimitError{RetryAfter: 7 * time.Second}
	}
	// Rotation consumes the presented live bearer; the old cookie becomes invalid.
	if previousToken != "" {
		if previousToken != f.issued {
			return c.Session{}, c.ErrUnauthorized
		}
		next := make([]byte, 32)
		if _, err := rand.Read(next); err != nil {
			return c.Session{}, c.ErrUnavailable
		}
		f.issued = base64.RawURLEncoding.EncodeToString(next)
	}
	return c.Session{ID: tenant, TenantID: tenant, CustomerID: tenant, Email: "buyer@example.test", Token: f.issued, ExpiresAt: time.Now().Add(time.Hour), IdleExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (f *fake) Current(_ context.Context, t, token string) (c.Session, error) {
	f.reads = append(f.reads, domainReceipt{operation: "current", tenant: t, token: token})
	if f.currentError != nil {
		return c.Session{}, f.currentError
	}
	if token == "" || token != f.issued || t != tenant {
		return c.Session{}, c.ErrUnauthorized
	}
	return c.Session{ID: tenant, TenantID: tenant, CustomerID: tenant, Email: "buyer@example.test", ExpiresAt: time.Now().Add(time.Hour), IdleExpiresAt: time.Now().Add(time.Hour)}, nil
}

// A temporary read failure does not prove the opaque bearer is invalid. Both
// session refresh and write preflight must retain it for a recovery retry.
func TestCustomerCookieSurvivesUnavailableStore(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			router, f := setup(t)
			bearer := &http.Cookie{Name: SessionCookie, Value: f.issued}
			live := request(router, http.MethodGet, sessionPath, "", nil, bearer)
			csrf := requireCustomerResponse(t, live, true)
			f.currentError = c.ErrUnavailable
			outage := request(router, method, sessionPath, "", map[string]string{"Origin": origin, "X-CSRF-Token": csrf}, bearer)
			if outage.Code != http.StatusServiceUnavailable {
				t.Fatalf("CUSTOMER_COOKIE_OUTAGE_RED: status %d", outage.Code)
			}
			for _, cookie := range outage.Result().Cookies() {
				if cookie.Name == SessionCookie {
					t.Fatal("CUSTOMER_COOKIE_OUTAGE_RED: temporary outage changed bearer")
				}
			}
			f.currentError = nil
			if recovered := request(router, http.MethodGet, sessionPath, "", nil, bearer); recovered.Code != http.StatusOK {
				t.Fatalf("same bearer did not recover: %d", recovered.Code)
			}
			f.currentError = c.ErrUnauthorized
			invalid := request(router, method, sessionPath, "", map[string]string{"Origin": origin, "X-CSRF-Token": csrf}, bearer)
			if invalid.Code != http.StatusUnauthorized {
				t.Fatalf("invalid bearer not rejected: %d", invalid.Code)
			}
			requireCustomerCookie(t, invalid, SessionCookie, true)
		})
	}
}
func (f *fake) Logout(_ context.Context, tenant, token string) error {
	f.record(domainReceipt{operation: "logout", tenant: tenant, token: token})
	f.issued = ""
	return nil
}
func randomToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		t.Fatal(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func setup(t *testing.T) (*httpapi.Router, *fake) {
	t.Helper()
	key := make([]byte, 32)
	if _, e := rand.Read(key); e != nil {
		t.Fatal(e)
	}
	f := &fake{issued: randomToken(t)}
	router := httpapi.New(nil, nil)
	for _, route := range New(Config{Service: f, Origin: origin, Secret: key}).Routes() {
		if e := router.Register(route.Spec, route.Handler); e != nil {
			t.Fatal(e)
		}
	}
	return router, f
}
func request(router http.Handler, method, path, body string, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, origin+path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.1:2345"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}
func challenge(t *testing.T, router http.Handler) (string, *http.Cookie) {
	t.Helper()
	w := request(router, "GET", sessionPath, "", nil)
	if w.Code != 200 {
		t.Fatalf("CUSTOMER_AUTH_HTTP_RED: challenge status %d", w.Code)
	}
	var b struct {
		CSRF string `json:"csrf_token"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &b); e != nil || b.CSRF == "" {
		t.Fatal("challenge absent")
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == ChallengeCookie {
			return b.CSRF, c
		}
	}
	t.Fatal("challenge cookie absent")
	return "", nil
}

func requireCustomerCookie(t *testing.T, w *httptest.ResponseRecorder, name string, cleared bool) *http.Cookie {
	t.Helper()
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name != name {
			continue
		}
		if !strings.HasPrefix(cookie.Name, "__Host-") || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" {
			t.Fatal("customer cookie policy")
		}
		if cleared {
			if cookie.Value != "" || cookie.MaxAge >= 0 || !cookie.Expires.Before(time.Now()) {
				t.Fatal("customer cookie was not cleared")
			}
		} else if cookie.Value == "" || cookie.MaxAge < 0 || !cookie.Expires.After(time.Now()) {
			t.Fatal("customer cookie was not live")
		}
		return cookie
	}
	t.Fatal("customer cookie absent")
	return nil
}

func requireCustomerResponse(t *testing.T, w *httptest.ResponseRecorder, authenticated bool) string {
	t.Helper()
	var response struct {
		Customer *c.Session `json:"customer"`
		CSRF     string     `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.CSRF == "" {
		t.Fatal("customer response or CSRF absent")
	}
	if authenticated {
		if response.Customer == nil || response.Customer.TenantID != tenant || response.Customer.CustomerID != tenant || response.Customer.Email != "buyer@example.test" {
			t.Fatal("authenticated customer response changed")
		}
	} else if response.Customer != nil {
		t.Fatal("anonymous challenge disclosed a customer")
	}
	return response.CSRF
}

// Every route proves its wire response and exact public-domain arguments.
func TestCustomerSevenRouteDomainContracts(t *testing.T) {
	credential := randomToken(t)
	challengeToken := randomToken(t)
	base := "/api/v1/t/" + tenant + "/customer"
	ip := netip.MustParseAddr("192.0.2.1")
	tests := []struct {
		name, method, path string
		fields             map[string]string
		status             int
		label              string
		want               domainReceipt
		cleared            bool
	}{
		{name: "register", method: "POST", path: base + "/registrations", fields: map[string]string{"email": "buyer@example.test", "password": credential}, status: 202, label: "verification_pending", want: domainReceipt{operation: "register", tenant: tenant, email: "buyer@example.test", credential: credential, ip: ip}},
		{name: "verify", method: "POST", path: base + "/verifications", fields: map[string]string{"token": challengeToken}, status: 204, want: domainReceipt{operation: "verify", tenant: tenant, token: challengeToken}},
		{name: "current", method: "GET", path: sessionPath, status: 200},
		{name: "login", method: "POST", path: sessionPath, fields: map[string]string{"email": "buyer@example.test", "password": credential}, status: 200, want: domainReceipt{operation: "login", tenant: tenant, email: "buyer@example.test", credential: credential, ip: ip}},
		{name: "logout", method: "DELETE", path: sessionPath, status: 204, want: domainReceipt{operation: "logout", tenant: tenant}, cleared: true},
		{name: "request-reset", method: "POST", path: base + "/password-resets", fields: map[string]string{"email": "buyer@example.test"}, status: 202, label: "accepted", want: domainReceipt{operation: "request-reset", tenant: tenant, email: "buyer@example.test", ip: ip}},
		{name: "reset", method: "POST", path: base + "/password-resets/confirm", fields: map[string]string{"token": challengeToken, "password": credential}, status: 204, want: domainReceipt{operation: "reset", tenant: tenant, token: challengeToken, credential: credential, ip: ip}, cleared: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, f := setup(t)
			var csrf string
			var cookie *http.Cookie
			want := test.want
			if test.method != "GET" {
				if test.name == "logout" {
					cookie = &http.Cookie{Name: SessionCookie, Value: f.issued}
					current := request(r, "GET", sessionPath, "", nil, cookie)
					if current.Code != 200 {
						t.Fatal("live logout session absent")
					}
					csrf = requireCustomerResponse(t, current, true)
					want.token = cookie.Value
				} else {
					csrf, cookie = challenge(t, r)
				}
			}
			var payload string
			if test.fields != nil {
				encoded, err := json.Marshal(test.fields)
				if err != nil {
					t.Fatal("route fixture encoding failed")
				}
				payload = string(encoded)
			}
			var cookies []*http.Cookie
			if cookie != nil {
				cookies = append(cookies, cookie)
			}
			w := request(r, test.method, test.path, payload, map[string]string{"Origin": origin, "X-CSRF-Token": csrf}, cookies...)
			if w.Code != test.status {
				t.Fatalf("route status %d, want %d", w.Code, test.status)
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Fatal("customer transport cache or referrer policy changed")
			}
			if test.method == "GET" {
				if f.calls != 0 || len(f.reads) != 0 {
					t.Fatal("anonymous session reached domain")
				}
				value := requireCustomerResponse(t, w, false)
				if requireCustomerCookie(t, w, ChallengeCookie, false).Value != value {
					t.Fatal("challenge cookie and CSRF differ")
				}
			} else if len(f.receipts) != 1 || f.receipts[0] != want {
				t.Fatal("route did not forward exact domain arguments")
			}
			if test.status == 204 && w.Body.Len() != 0 {
				t.Fatal("no-content response contained a body")
			}
			if test.label != "" {
				var status struct {
					Status string `json:"status"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Status != test.label {
					t.Fatal("generic route response changed")
				}
			}
			if test.name == "login" {
				requireCustomerResponse(t, w, true)
				if requireCustomerCookie(t, w, SessionCookie, false).Value != f.issued || strings.Contains(w.Body.String(), f.issued) {
					t.Fatal("login bearer transport changed")
				}
				requireCustomerCookie(t, w, ChallengeCookie, true)
			}
			if test.cleared {
				requireCustomerCookie(t, w, SessionCookie, true)
				requireCustomerCookie(t, w, ChallengeCookie, true)
			}
			if test.name == "logout" {
				if len(f.reads) != 2 || f.reads[1] != (domainReceipt{operation: "current", tenant: tenant, token: want.token}) {
					t.Fatal("logout skipped current-session admission")
				}
				if request(r, "GET", sessionPath, "", nil, cookie).Code != 401 {
					t.Fatal("logged-out bearer remained live")
				}
			}
		})
	}
}

func TestCustomerOriginAndSignedCSRF(t *testing.T) {
	r, f := setup(t)
	csrf, cookie := challenge(t, r)
	for _, test := range []struct {
		name, source, token string
		cookie              bool
	}{{"missing", "", "", false}, {"foreign", "https://evil.example.test", csrf, true}, {"wrong", origin, "unsigned", true}, {"no-cookie", origin, csrf, false}} {
		t.Run(test.name, func(t *testing.T) {
			var cookies []*http.Cookie
			if test.cookie {
				cookies = append(cookies, cookie)
			}
			w := request(r, "POST", sessionPath, `{"email":"buyer@example.test","password":"ignored"}`, map[string]string{"Origin": test.source, "X-CSRF-Token": test.token}, cookies...)
			if w.Code != 403 {
				t.Errorf("CSRF admission status %d", w.Code)
			}
		})
	}
	if f.calls != 0 {
		t.Fatal("CSRF rejection reached domain")
	}
}
func TestCustomerCookieFlagsRotationAndNoBearerJSON(t *testing.T) {
	r, f := setup(t)
	csrf, cookie := challenge(t, r)
	credential := randomToken(t)
	body, _ := json.Marshal(map[string]string{"email": "buyer@example.test", "password": credential})
	w := request(r, "POST", sessionPath, string(body), map[string]string{"Origin": origin, "X-CSRF-Token": csrf}, cookie)
	if w.Code != 200 {
		t.Fatalf("login status %d", w.Code)
	}
	if strings.Contains(w.Body.String(), f.issued) {
		t.Fatal("session bearer leaked JSON")
	}
	var found bool
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookie {
			found = true
			if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" || c.Value != f.issued {
				t.Fatal("customer cookie policy")
			}
		}
	}
	if !found {
		t.Fatal("session cookie absent")
	}
	firstCookie := requireCustomerCookie(t, w, SessionCookie, false)
	firstCSRF := requireCustomerResponse(t, w, true)
	requireCustomerCookie(t, w, ChallengeCookie, true)
	if len(f.receipts) != 1 || f.receipts[0].previousToken != "" {
		t.Fatal("first login used a previous bearer")
	}
	rotated := request(r, "POST", sessionPath, string(body), map[string]string{"Origin": origin, "X-CSRF-Token": firstCSRF}, firstCookie)
	if rotated.Code != 200 {
		t.Fatalf("rotation status %d", rotated.Code)
	}
	secondCookie := requireCustomerCookie(t, rotated, SessionCookie, false)
	secondCSRF := requireCustomerResponse(t, rotated, true)
	requireCustomerCookie(t, rotated, ChallengeCookie, true)
	if firstCookie.Value == secondCookie.Value || firstCSRF == secondCSRF || secondCookie.Value != f.issued {
		t.Fatal("session cookie or CSRF did not rotate")
	}
	if len(f.receipts) != 2 || f.receipts[1].previousToken != firstCookie.Value {
		t.Fatal("rotation did not forward previous bearer")
	}
	if strings.Contains(rotated.Body.String(), firstCookie.Value) || strings.Contains(rotated.Body.String(), secondCookie.Value) {
		t.Fatal("rotated bearer leaked JSON")
	}
	if request(r, "GET", sessionPath, "", nil, firstCookie).Code != 401 {
		t.Fatal("old bearer survived rotation")
	}
	current := request(r, "GET", sessionPath, "", nil, secondCookie)
	if current.Code != 200 || requireCustomerResponse(t, current, true) != secondCSRF {
		t.Fatal("rotated session or CSRF was not current")
	}
	if requireCustomerCookie(t, current, SessionCookie, false).Value != secondCookie.Value {
		t.Fatal("current session changed bearer")
	}
	calls := f.calls
	stale := request(r, "POST", sessionPath, string(body), map[string]string{"Origin": origin, "X-CSRF-Token": firstCSRF}, secondCookie)
	if stale.Code != 403 || f.calls != calls {
		t.Fatal("previous session CSRF reached domain")
	}
}
func TestCustomerStrictFieldsAndRetryAfter(t *testing.T) {
	r, f := setup(t)
	csrf, cookie := challenge(t, r)
	headers := map[string]string{"Origin": origin, "X-CSRF-Token": csrf}
	for _, body := range []string{`{"email":"a@example.test","email":"b@example.test","password":"ignored"}`, `{"Email":"a@example.test","password":"ignored"}`, `{"email":"a@example.test","password":"ignored","customer_id":"forged"}`} {
		w := request(r, "POST", sessionPath, body, headers, cookie)
		if w.Code != 400 {
			t.Errorf("strict body status %d", w.Code)
		}
	}
	if f.calls != 0 {
		t.Fatal("invalid JSON reached domain")
	}
	f.limited = true
	credential := randomToken(t)
	body, _ := json.Marshal(map[string]string{"email": "buyer@example.test", "password": credential})
	w := request(r, "POST", sessionPath, string(body), headers, cookie)
	if w.Code != 429 || w.Header().Get("Retry-After") != "7" {
		t.Fatal("rate response lost")
	}
}
func TestCustomerCookieAmbiguityAndTenantIsolation(t *testing.T) {
	r, f := setup(t)
	cookie := &http.Cookie{Name: SessionCookie, Value: f.issued}
	w := request(r, "GET", sessionPath, "", nil, cookie, cookie)
	if w.Code != 401 {
		t.Fatalf("duplicate cookie status %d", w.Code)
	}
	w = request(r, "GET", strings.Replace(sessionPath, tenant, "22222222-2222-4222-8222-222222222222", 1), "", nil, cookie)
	if w.Code != 401 {
		t.Fatalf("cross tenant session status %d", w.Code)
	}
}

func TestCustomerForeignChallengeOriginAndExpiredCSRF(t *testing.T) {
	r, f := setup(t)
	w := request(r, "GET", sessionPath, "", map[string]string{"Origin": "https://foreign.example.test"})
	if w.Code != 403 || len(w.Result().Cookies()) != 0 {
		t.Fatal("foreign challenge origin accepted")
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	now := time.Now()
	h := New(Config{Service: f, Origin: origin, Secret: key, Now: func() time.Time { return now }})
	csrf, err := h.challenge(tenant)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Minute)
	req := httptest.NewRequest("POST", origin+sessionPath, nil)
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(&http.Cookie{Name: ChallengeCookie, Value: csrf})
	if h.writeCSRF(req, tenant, "") {
		t.Fatal("expired CSRF accepted")
	}
	for _, unsafe := range []string{"https://shop.example.test?", "https://:443", "https://shop.example.test/", "https://shop.example.test#fragment"} {
		if New(Config{Service: f, Origin: unsafe, Secret: key}).configured {
			t.Fatal("ambiguous configured origin accepted")
		}
	}
}

func TestCustomerResolverRequiresLiveTenantSessionAndCSRF(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	f := &fake{issued: randomToken(t)}
	h := New(Config{Service: f, Origin: origin, Secret: key})
	req := httptest.NewRequest("GET", origin+sessionPath, nil)
	req.SetPathValue("tenant_id", tenant)
	actor, err := h.Resolve(context.Background(), req)
	if err != nil || actor.CustomerID != "" {
		t.Fatal("guest seam changed")
	}
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: f.issued})
	actor, err = h.Resolve(context.Background(), req)
	if err != nil || actor.CustomerID != tenant {
		t.Fatal("verified context absent")
	}
	req.Method = "PUT"
	if _, err = h.Resolve(context.Background(), req); err == nil {
		t.Fatal("cookie storefront write bypassed CSRF")
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("X-CSRF-Token", h.sessionCSRF(tenant, f.issued))
	if _, err = h.Resolve(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.SetPathValue("tenant_id", "22222222-2222-4222-8222-222222222222")
	req.Method = "GET"
	actor, err = h.Resolve(context.Background(), req)
	if err != nil || actor != (c.Actor{}) {
		t.Fatal("invalid cross-tenant bearer did not return a guest with zero customer authority")
	}
}
