// Responsibility: pin guest/customer coexistence through all composed browser routes.
// Dependencies: bootstrap HTTP and public ports; caller: CI. Invalid cookies never become guests.
package bootstrap_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	checkouthttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func (s *checkoutWiringService) Preview(_ context.Context, a checkoutcontract.AuthorizedActor, _ checkoutcontract.PreviewInput) (checkoutcontract.Preview, error) {
	s.calls++
	s.actor = a
	return checkoutcontract.Preview{Currency: "USD", TotalMinor: 1200}, nil
}
func (s *checkoutWiringService) Get(ctx context.Context, a checkoutcontract.AuthorizedActor, _ string, _ string) (checkoutcontract.Result, error) {
	return s.Start(ctx, a, checkoutcontract.StartInput{})
}
func TestComposedGuestCustomerAllBrowserOperations(t *testing.T) {
	for _, op := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"start", "POST", "", checkoutWiringBody, 201},
		{"preview", "POST", "/preview", `{"cart_id":"44444444-4444-4444-8444-444444444444","shipping_address":{"country":"US","postal_code":"00001"}}`, 200},
		{"get", "GET", "/55555555-5555-4555-8555-555555555555", "", 200},
	} {
		for _, mode := range []string{"guest", "customer", "invalid_cookie", "invalid_cookie_no_port", "staff"} {
			t.Run(op.name+"/"+mode, func(t *testing.T) {
				_, port, svc, cookie := checkoutWiringFixture(t)
				key := []byte(strings.Repeat("k", 32))
				token := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("g", 32)))
				reads := 0
				resolver, err := checkouthttp.NewGuestResolver(checkouthttp.GuestConfig{TenantID: checkoutWiringTenant, Origin: checkoutWiringOrigin, Key: key, VerifyCart: func(context.Context, checkoutcontract.AuthorizedActor, string) error { reads++; return nil }})
				if err != nil {
					t.Fatal(err)
				}
				config := bootstrap.Config{Customer: port, CustomerOrigin: checkoutWiringOrigin, CustomerSecret: []byte(strings.Repeat("c", 32)), Authenticator: rejectStaffAuth{t: t}, Checkout: &checkouthttp.Config{Service: svc, Resolver: resolver, Origin: checkoutWiringOrigin}}
				if mode == "invalid_cookie_no_port" {
					config.Customer = nil
				}
				server, err := bootstrap.New(config)
				if err != nil {
					t.Fatal(err)
				}
				mac := hmac.New(sha256.New, key)
				_, _ = mac.Write([]byte("checkout:" + checkoutWiringTenant + ":" + token + ":" + checkoutWiringOrigin))
				r := checkoutWiringRequest(checkoutWiringTenant, base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil)
				r.Header.Set("X-Cart-ID", "44444444-4444-4444-8444-444444444444")
				r.Header.Set("X-Cart-Token", token)
				switch mode {
				case "customer":
					r.AddCookie(cookie)
					r.Header.Del("X-Cart-Token")
					r.Header.Set("X-CSRF-Token", checkoutWiringCSRF(t, server.Handler, cookie, checkoutWiringTenant))
				case "invalid_cookie", "invalid_cookie_no_port":
					r.AddCookie(cookie)
					delete(port.sessions, checkoutWiringTenant)
				case "staff":
					r.Header.Del("X-Cart-Token")
					r.Header.Set("Authorization", "Bearer staff")
					r.AddCookie(&http.Cookie{Name: "__Host-osc_admin_session", Value: token})
				}
				port.calls = 0
				r.URL.Path += op.path
				r.Method, r.Body = op.method, ioBody(op.body)
				if op.name == "start" && mode != "customer" {
					r.Body = ioBody(strings.TrimSuffix(checkoutWiringBody, "}") + `,"buyer_contact":{"email":"guest@example.test"}}`)
				}
				w := httptest.NewRecorder()
				server.Handler.ServeHTTP(w, r)
				if mode == "guest" || mode == "customer" {
					if w.Code != op.status || svc.calls != 1 || svc.actor.Kind() != mode {
						t.Fatalf("status=%d calls=%d actor=%s body=%s", w.Code, svc.calls, svc.actor.Kind(), w.Body.String())
					}
					if mode == "guest" && reads != 1 || mode == "customer" && (reads != 0 || port.calls != 1) {
						t.Fatal("resolver branch/call drift")
					}
				} else if w.Code < 400 || svc.calls != 0 || reads != 0 {
					t.Fatalf("denial failed: %d calls=%d reads=%d", w.Code, svc.calls, reads)
				}
			})
		}
	}
}

func ioBody(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }
