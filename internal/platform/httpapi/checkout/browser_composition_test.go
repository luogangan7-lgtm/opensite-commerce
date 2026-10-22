// Responsibility: pin all browser entry points and staff extension forwarding.
// Dependencies: checkout public HTTP composition; called by CI without DB doubles.
// Invariant: storefront never falls back to staff, whose three extensions survive.
package checkout_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	checkout "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
)

type browserStaff struct{ calls int }

func (s *browserStaff) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	s.calls++
	return ctx, nil
}
func (*browserStaff) AuthenticationResponse(_ context.Context, w http.ResponseWriter, _ *http.Request, _ httpapi.RouteSpec) {
	w.Header().Set("X-Staff-Witness", "yes")
}
func TestBrowserCompositionRoutesAndStaffAuthority(t *testing.T) {
	h, err := checkout.New(checkout.Config{})
	if err != nil {
		t.Fatal(err)
	}
	staff := &browserStaff{}
	auth := h.Authenticator(staff)
	want := map[string]string{"createCheckout": "POST /api/v1/t/{tenant_id}/checkout", "previewCheckout": "POST /api/v1/t/{tenant_id}/checkout/preview", "getCheckout": "GET /api/v1/t/{tenant_id}/checkout/{checkout_id}"}
	for _, route := range h.Routes() {
		spec := route.Spec
		if want[spec.OperationID] != spec.Method+" "+spec.Pattern || spec.Access != httpapi.Storefront || spec.Scope != "" {
			t.Fatalf("unexpected browser route: %+v", spec)
		}
		delete(want, spec.OperationID)
		r := httptest.NewRequest(spec.Method, "/", nil)
		if _, err := auth.Authenticate(r.Context(), r, spec); err == nil || staff.calls != 0 {
			t.Fatal("staff admitted to storefront")
		}

	}
	if len(want) != 0 {
		t.Fatalf("BROWSER_ROUTES_RED: missing %v", want)
	}
	spec := httpapi.RouteSpec{OperationID: "staffProbe", Access: httpapi.AdminOrAPIKey}
	r := httptest.NewRequest("GET", "/", nil)
	responder, ok := auth.(httpapi.AuthenticationResponder)
	if !ok {
		t.Fatal("STAFF_RESPONSE_RED: wrapper lost AuthenticationResponder")
	}
	w := httptest.NewRecorder()
	responder.AuthenticationResponse(r.Context(), w, r, spec)
	if w.Header().Get("X-Staff-Witness") != "yes" {
		t.Fatal("staff response lost")
	}
}
