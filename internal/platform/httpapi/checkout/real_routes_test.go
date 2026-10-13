// Checkout's browser read/preview contract must be present in explicit composition.
// Dependencies: HTTP route metadata only; no database or provider side effects.
// Called by the checkout route gate; Fake remains opt-in and absent by default.
package checkout_test

import (
	"net/http"
	"testing"

	checkout "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
)

func TestCheckoutBrowserRoutesExcludeFakeByDefault(t *testing.T) {
	h, err := checkout.New(checkout.Config{Service: &service{}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"createCheckout": "POST /api/v1/t/{tenant_id}/checkout",
		"previewCheckout": "POST /api/v1/t/{tenant_id}/checkout/preview",
		"getCheckout": "GET /api/v1/t/{tenant_id}/checkout/{checkout_id}",
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
