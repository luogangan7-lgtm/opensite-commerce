// Responsibilities: reconcile actual production RouteSpec registrations with
// implemented OpenAPI operations in both directions, including transport/auth.
// Dependencies: bootstrap, platform/httpapi, kin-openapi; called by Go/CI gates.
// Planned operations and test fixture routes must never masquerade as implemented.
package bootstrap_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func reconcileRoutes(doc *openapi3.T, specs []httpapi.RouteSpec) error {
	registered := make(map[string]httpapi.RouteSpec)
	for _, spec := range specs {
		if _, exists := registered[spec.OperationID]; exists {
			return fmt.Errorf("duplicate registered operation %s", spec.OperationID)
		}
		registered[spec.OperationID] = spec
	}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			spec, exists := registered[op.OperationID]
			if op.Extensions["x-opensite-status"] != "implemented" {
				if exists {
					return fmt.Errorf("registered operation %s is planned", op.OperationID)
				}
				continue
			}
			if !exists {
				return fmt.Errorf("implemented operation %s has no registered handler", op.OperationID)
			}
			if spec.Method != strings.ToUpper(method) || spec.Pattern != path || string(spec.Access) != op.Extensions["x-opensite-access"] || spec.Scope != scope(op) {
				return fmt.Errorf("RouteSpec/OpenAPI transport or authorization mismatch for %s", op.OperationID)
			}
			delete(registered, op.OperationID)
		}
	}
	for id := range registered {
		return fmt.Errorf("registered operation %s is missing from implemented OpenAPI", id)
	}
	return nil
}

func scope(op *openapi3.Operation) string {
	s, _ := op.Extensions["x-required-scope"].(string)
	return s
}

func TestImplementedRouteContract(t *testing.T) {
	server, err := bootstrap.New(bootstrap.Config{})
	if err != nil {
		t.Fatal(err)
	}
	router, ok := server.Handler.(*httpapi.Router)
	if !ok {
		t.Fatal("bootstrap must expose the registered RouteSpec router")
	}
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileRoutes(doc, router.RouteSpecs()); err != nil {
		t.Fatal(err)
	}
}

func TestRouteContractRejectsDrift(t *testing.T) {
	// Positive control uses an independent tiny contract, then mutates each side.
	const spec = `{"openapi":"3.1.0","info":{"title":"probe","version":"1"},"paths":{"/probe":{"get":{"operationId":"probe","x-opensite-status":"implemented","x-opensite-access":"Public","responses":{"204":{"description":"ok"}}}}}}`
	for _, change := range []string{"missing spec", "missing route", "planned", "method", "path", "access", "scope", "duplicate route"} {
		t.Run(change, func(t *testing.T) {
			doc, err := openapi3.NewLoader().LoadFromData([]byte(spec))
			if err != nil {
				t.Fatal(err)
			}
			routes := []httpapi.RouteSpec{{OperationID: "probe", Method: "GET", Pattern: "/probe", Access: httpapi.Public}}
			if err := reconcileRoutes(doc, routes); err != nil {
				t.Fatalf("positive control: %v", err)
			}
			switch change {
			case "missing spec":
				doc.Paths.Delete("/probe")
			case "missing route":
				routes = nil
			case "planned":
				doc.Paths.Value("/probe").Get.Extensions["x-opensite-status"] = "planned"
			case "method":
				routes[0].Method = "POST"
			case "path":
				routes[0].Pattern = "/other"
			case "access":
				routes[0].Access = httpapi.AdminSession
			case "scope":
				routes[0].Scope = "catalog.write"
			case "duplicate route":
				routes = append(routes, routes[0])
			}
			if err := reconcileRoutes(doc, routes); err == nil {
				t.Fatal("contract drift accepted")
			}
		})
	}
}

func TestRouteSpecsAreSuccessfulRegistrationSnapshots(t *testing.T) {
	router := httpapi.New(nil, nil)
	spec := httpapi.RouteSpec{OperationID: "probe", Method: "GET", Pattern: "/probe", Access: httpapi.Public}
	if err := router.Register(spec, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})); err != nil {
		t.Fatal(err)
	}
	invalid := spec
	invalid.OperationID = "invalid"
	invalid.Access = "unknown"
	if err := router.Register(invalid, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})); err == nil {
		t.Fatal("invalid route registered")
	}
	snapshot := router.RouteSpecs()
	if len(snapshot) != 1 || snapshot[0] != spec {
		t.Fatalf("unsuccessful route leaked: %v", snapshot)
	}
	snapshot[0].OperationID = "tampered"
	if got := router.RouteSpecs(); len(got) != 1 || got[0] != spec {
		t.Fatalf("snapshot mutated registry: %v", got)
	}
}

// Default construction installs real handlers with an unavailable repository.
// It cannot pretend a cart exists, and staff authentication never admits carts.
func TestDefaultCartWiringIsUnavailableAndSeparateFromStaff(t *testing.T) {
	server, err := bootstrap.New(bootstrap.Config{Authenticator: rejectStaffAuth{t: t}})
	if err != nil {
		t.Fatal(err)
	}
	router := server.Handler.(*httpapi.Router)
	ids := map[string]bool{}
	for _, spec := range router.RouteSpecs() {
		ids[spec.OperationID] = true
	}
	for _, id := range []string{"createCart", "getCart", "setCartLine", "deleteCartLine"} {
		if !ids[id] {
			t.Fatalf("missing actual cart operation %s", id)
		}
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/t/11111111-1111-4111-8111-111111111111/carts", strings.NewReader(`{"currency":"USD"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer staff")
	req.Header.Set("Cookie", "__Host-osc_admin_session=staff")
	server.Handler.ServeHTTP(w, req)
	if w.Code != 503 || w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("nil repository faked business result: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cart failure cache policy missing")
	}
}

type rejectStaffAuth struct{ t *testing.T }

func (a rejectStaffAuth) Authenticate(context.Context, *http.Request, httpapi.RouteSpec) (context.Context, error) {
	a.t.Fatal("staff auth was used for storefront cart")
	return nil, nil
}
