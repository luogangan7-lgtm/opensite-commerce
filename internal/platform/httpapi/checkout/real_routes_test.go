// Checkout's browser read/preview contract must be present in explicit composition.
// Dependencies: HTTP route metadata only; no database or provider side effects.
// Called by the checkout route gate; Fake remains opt-in and absent by default.
package checkout_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	contract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	checkout "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
)

type browserService struct {
	*service
	previews, reads int
}

func (s *browserService) Preview(_ context.Context, _ contract.AuthorizedActor, _ contract.PreviewInput) (contract.Preview, error) {
	s.previews++
	return contract.Preview{SubtotalMinor: 1200, TaxMinor: 100, ShippingMinor: 50, TotalMinor: 1350, Currency: "USD", ShippingOptions: []contract.ShippingOption{{RateID: cart, Method: "ground", AmountMinor: 50}}}, nil
}
func (s *browserService) Get(_ context.Context, _ contract.AuthorizedActor, _, _ string) (contract.Result, error) {
	s.reads++
	return result(), nil
}

func TestCheckoutBrowserRoutesExcludeFakeByDefault(t *testing.T) {
	h, err := checkout.New(checkout.Config{Service: &service{}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"createCheckout":  "POST /api/v1/t/{tenant_id}/checkout",
		"previewCheckout": "POST /api/v1/t/{tenant_id}/checkout/preview",
		"getCheckout":     "GET /api/v1/t/{tenant_id}/checkout/{checkout_id}",
	}
	for _, route := range h.Routes() {
		if route.Spec.OperationID == checkout.FakeOperation {
			t.Fatal("Fake webhook mounted without explicit test mode")
		}
		got := route.Spec.Method + " " + route.Spec.Pattern
		if want[route.Spec.OperationID] != got {
			t.Fatalf("unexpected checkout route %q %q", route.Spec.OperationID, got)
		}
		if route.Spec.Access != "Storefront" || route.Spec.Scope != "" || (route.Spec.Method != http.MethodGet && route.Spec.Method != http.MethodPost) {
			t.Fatalf("unsafe checkout access: %+v", route.Spec)
		}
		delete(want, route.Spec.OperationID)
	}
	if len(want) != 0 {
		t.Fatalf("missing checkout browser routes: %+v", want)
	}
}

func TestCheckoutPreviewRequiresMatchingCartAndReturnsDecimalAmounts(t *testing.T) {
	s := &browserService{service: &service{}}
	r := router(t, checkout.Config{Service: s, Resolver: resolver{actor: actor(t, tenant, "guest")}, Origin: origin})
	path := "/api/v1/t/" + tenant + "/checkout/preview"
	wrong := request(r, path, `{"cart_id":"`+orderID+`","shipping_address":{"country":"US","postal_code":"00001"}}`, nil)
	checkProblem(t, wrong, http.StatusNotFound)
	if s.previews != 0 {
		t.Fatal("mismatched cart reached preview")
	}
	w := request(r, path, `{"cart_id":"`+cart+`","shipping_address":{"country":"US","postal_code":"00001"}}`, nil)
	if w.Code != http.StatusOK || s.previews != 1 {
		t.Fatalf("preview failed: status=%d calls=%d body=%s", w.Code, s.previews, w.Body.String())
	}
	var body struct {
		TotalMinor      string `json:"total_minor"`
		ShippingOptions []struct {
			AmountMinor string `json:"amount_minor"`
		} `json:"shipping_options"`
	}
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.TotalMinor != "1350" || len(body.ShippingOptions) != 1 || body.ShippingOptions[0].AmountMinor != "50" {
		t.Fatalf("preview amount contract drift: %s", w.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, "/api/v1/t/"+tenant+"/checkout/"+checkoutID, nil)
	get.Header.Set("Origin", origin)
	get.Header.Set("X-CSRF-Token", "fixture-session-csrf")
	get.Header.Set("X-Cart-ID", cart)
	get.Header.Set("X-Cart-Token", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	got := httptest.NewRecorder()
	r.ServeHTTP(got, get)
	if got.Code != http.StatusOK || s.reads != 1 {
		t.Fatalf("completion read failed: status=%d calls=%d body=%s", got.Code, s.reads, got.Body.String())
	}
}
