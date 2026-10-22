// Responsibilities: preserve staff identity through optional storefront composition.
// Dependencies: real bootstrap/Router and deterministic admitted staff test double.
// Invariant: enabling checkout cannot strip actor or write authorization extensions.
package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
)

type c02cGuardAuth struct{ cmsStaffAuth }

// A future response-admission extension compiles against the old seam; the
// header only appears once the Router and every composition wrapper forward it.
func (c02cGuardAuth) AuthenticationResponse(_ context.Context, w http.ResponseWriter, _ *http.Request, _ httpapi.RouteSpec) {
	w.Header().Set("X-C02c-Response-Witness", "admitted")
}

func (c02cGuardAuth) ResolveTransactionAuthorization(context.Context, *http.Request, httpapi.RouteSpec) (func(context.Context, *db.Queries) error, error) {
	return nil, apperrors.New(403, "forbidden", "Write authority unavailable")
}
func TestC02cCheckoutPreservesStaffExtensions(t *testing.T) {
	// Pin the inner wrapper too; the outer customer wrapper has a separate projection.
	h, err := checkout.New(checkout.Config{})
	if err != nil {
		t.Fatal(err)
	}
	responder, ok := h.Authenticator(c02cGuardAuth{}).(httpapi.AuthenticationResponder)
	if !ok {
		t.Fatal("STAFF_RESPONSE_RED: inner response projection lost")
	}
	response := httptest.NewRecorder()
	responder.AuthenticationResponse(context.Background(), response, httptest.NewRequest("GET", "/", nil), httpapi.RouteSpec{OperationID: "staffProbe"})
	if response.Header().Get("X-C02c-Response-Witness") != "admitted" {
		t.Fatal("inner response projection lost")
	}
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			called := false
			route := httpapi.Route{Spec: httpapi.RouteSpec{OperationID: "c02cComposition", Method: method, Pattern: "/api/v1/t/{tenant_id}/authority-probe", Access: httpapi.AdminOrAPIKey, Scope: "catalog:write"}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if _, err := httpapi.AuthorizedActor(r.Context()); err != nil {
					httpapi.WriteProblem(w, r, err)
					return
				}
				w.WriteHeader(204)
			})}
			s, err := bootstrap.New(bootstrap.Config{Authenticator: c02cGuardAuth{}, Checkout: &checkout.Config{}, Routes: []httpapi.Route{route}})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			s.Handler.ServeHTTP(w, httptest.NewRequest(method, "/api/v1/t/11111111-1111-4111-8111-111111111111/authority-probe", nil))
			if method == "GET" && w.Code != 204 {
				t.Fatalf("staff actor lost in checkout composition: %d", w.Code)
			}
			if method == "POST" && (w.Code != 403 || called) {
				t.Fatalf("staff write guard lost in checkout composition: %d called %v", w.Code, called)
			}
			if method == "GET" && w.Header().Get("X-C02c-Response-Witness") != "admitted" {
				t.Fatal("STAFF_RENEWAL_COMPOSITION_RED: admitted response extension was lost")
			}
			if method == "POST" && w.Header().Get("X-C02c-Response-Witness") != "" {
				t.Fatal("denied write emitted authenticated response")
			}
		})
	}
}
