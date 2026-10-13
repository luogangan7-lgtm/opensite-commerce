// Responsibility: pin customer and guest checkout coexistence at bootstrap admission.
// Dependencies: public checkout/customer ports and real HTTP routes; caller: CI.
// Invariants: a customer cookie never falls back to guest, and guest proof never admits a customer.
package bootstrap_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	customercontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	checkouthttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/customerhttp"
)

const dualGuestCart = "44444444-4444-4444-8444-444444444444"
const dualGuestBody = `{"cart_id":"` + dualGuestCart + `","buyer_contact":{"email":"guest@example.test"},"shipping_address":{"country":"US","address1":"Fixture road","city":"Fixture city","postal_code":"00001"}}`

type dualCheckoutFixture struct {
	http     http.Handler
	customer *checkoutWiringCustomerPort
	service  *checkoutWiringService
	cookie   *http.Cookie
	guest    string
	proof    string
	reads    *int
}

func dualCheckout(t *testing.T, customerOrigin string) dualCheckoutFixture {
	t.Helper()
	guestKey, customerKey := make([]byte, 32), make([]byte, 32)
	for i := range guestKey {
		guestKey[i], customerKey[i] = 1, 2
	}
	guestToken := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("g", 32)))
	customerToken := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("c", 32)))
	guestHash := sha256.Sum256([]byte(guestToken))
	reads := 0
	resolver, err := checkouthttp.NewGuestResolver(checkouthttp.GuestConfig{TenantID: checkoutWiringTenant, Origin: checkoutWiringOrigin, Key: guestKey, VerifyCart: func(_ context.Context, actor checkoutcontract.AuthorizedActor, cartID string) error {
		reads++
		if actor.Kind() != "guest" || actor.TenantID() != checkoutWiringTenant || actor.ID() != hex.EncodeToString(guestHash[:]) || cartID != dualGuestCart {
			t.Fatal("guest resolver received incorrect cart ownership")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	port := &checkoutWiringCustomerPort{wiringCustomer: wiringCustomer{token: customerToken}, sessions: map[string]customercontract.Session{checkoutWiringTenant: {ID: checkoutWiringCustomer, TenantID: checkoutWiringTenant, CustomerID: checkoutWiringCustomer, Email: "buyer@example.test", ExpiresAt: time.Now().Add(time.Hour), IdleExpiresAt: time.Now().Add(time.Hour)}}}
	service := &checkoutWiringService{}
	server, err := bootstrap.New(bootstrap.Config{Customer: port, CustomerOrigin: customerOrigin, CustomerSecret: customerKey, Checkout: &checkouthttp.Config{Service: service, Origin: checkoutWiringOrigin, Resolver: resolver}})
	if err != nil {
		if customerOrigin != checkoutWiringOrigin {
			if !strings.Contains(err.Error(), "origin") {
				t.Fatalf("origin mismatch failed for another reason: %v", err)
			}
			return dualCheckoutFixture{}
		}
		t.Fatal(err)
	}
	if customerOrigin != checkoutWiringOrigin {
		t.Fatal("mismatched customer and guest origins admitted")
	}
	mac := hmac.New(sha256.New, guestKey)
	_, _ = mac.Write([]byte("checkout:" + checkoutWiringTenant + ":" + guestToken + ":" + checkoutWiringOrigin))
	return dualCheckoutFixture{http: server.Handler, customer: port, service: service, cookie: &http.Cookie{Name: customerhttp.SessionCookie, Value: customerToken}, guest: guestToken, proof: base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), reads: &reads}
}

func (f dualCheckoutFixture) guestRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, checkoutWiringOrigin+"/api/v1/t/"+checkoutWiringTenant+"/checkout", strings.NewReader(dualGuestBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "dual-guest-checkout-1")
	r.Header.Set("Origin", checkoutWiringOrigin)
	r.Header.Set("X-CSRF-Token", f.proof)
	r.Header.Set("X-Cart-ID", dualGuestCart)
	r.Header.Set("X-Cart-Token", f.guest)
	return r
}

func TestCustomerAndGuestCheckoutCoexist(t *testing.T) {
	f := dualCheckout(t, checkoutWiringOrigin)
	w := httptest.NewRecorder()
	f.http.ServeHTTP(w, f.guestRequest())
	if w.Code != 201 || f.service.calls != 1 || f.service.actor.Kind() != "guest" || *f.reads != 1 || f.customer.calls != 0 {
		t.Fatalf("guest checkout changed: status=%d starts=%d guest_reads=%d customer_reads=%d", w.Code, f.service.calls, *f.reads, f.customer.calls)
	}
	csrf := checkoutWiringCSRF(t, f.http, f.cookie, checkoutWiringTenant)
	f.customer.calls = 0
	w = httptest.NewRecorder()
	f.http.ServeHTTP(w, checkoutWiringRequest(checkoutWiringTenant, csrf, f.cookie))
	if w.Code != 201 || f.service.calls != 2 || f.service.actor.Kind() != "customer" || f.service.actor.ID() != checkoutWiringCustomer || f.customer.calls != 1 || *f.reads != 1 {
		t.Fatalf("customer checkout changed: status=%d starts=%d kind=%s customer_reads=%d guest_reads=%d", w.Code, f.service.calls, f.service.actor.Kind(), f.customer.calls, *f.reads)
	}
}

func TestCustomerCookieNeverFallsBackToGuestCheckout(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*dualCheckoutFixture, *http.Request)
	}{
		{"revoked_cookie", func(f *dualCheckoutFixture, r *http.Request) { delete(f.customer.sessions, checkoutWiringTenant) }},
		{"mixed_credentials", func(f *dualCheckoutFixture, r *http.Request) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := dualCheckout(t, checkoutWiringOrigin)
			csrf := checkoutWiringCSRF(t, f.http, f.cookie, checkoutWiringTenant)
			f.customer.calls = 0
			r := f.guestRequest()
			r.Header.Set("X-CSRF-Token", csrf)
			r.AddCookie(f.cookie)
			tc.setup(&f, r)
			w := httptest.NewRecorder()
			f.http.ServeHTTP(w, r)
			if w.Code != 404 || f.service.calls != 0 || *f.reads != 0 || f.customer.calls != 1 {
				t.Fatalf("customer cookie fell back to guest: status=%d starts=%d guest_reads=%d customer_reads=%d", w.Code, f.service.calls, *f.reads, f.customer.calls)
			}
		})
	}
}

func TestCustomerCannotUseGuestOnlyCheckoutReads(t *testing.T) {
	f := dualCheckout(t, checkoutWiringOrigin)
	csrf := checkoutWiringCSRF(t, f.http, f.cookie, checkoutWiringTenant)
	f.customer.calls = 0
	for _, methodPath := range [][2]string{{http.MethodPost, "/api/v1/t/" + checkoutWiringTenant + "/checkout/preview"}, {http.MethodGet, "/api/v1/t/" + checkoutWiringTenant + "/checkout/55555555-5555-4555-8555-555555555555"}} {
		r := httptest.NewRequest(methodPath[0], checkoutWiringOrigin+methodPath[1], nil)
		r.Header.Set("Origin", checkoutWiringOrigin)
		r.Header.Set("X-CSRF-Token", csrf)
		r.AddCookie(f.cookie)
		w := httptest.NewRecorder()
		f.http.ServeHTTP(w, r)
		if w.Code != 404 || f.customer.calls != 0 || *f.reads != 0 {
			t.Fatalf("customer cookie admitted guest-only read: route=%s status=%d customer_reads=%d guest_reads=%d", methodPath[1], w.Code, f.customer.calls, *f.reads)
		}
	}
}

func TestCustomerGuestOriginsMustMatch(t *testing.T) {
	_ = dualCheckout(t, "https://different.example.test")
}
