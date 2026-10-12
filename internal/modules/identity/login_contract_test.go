// Responsibility: reconcile real identity handler registrations with full OpenAPI.
// Dependencies: identity transport, bootstrap and kin-openapi; caller: Go gates.
// Invariants: no database/secrets; real handlers supply specs; both directions
// reject drift. Repository-dependent authentication remains a separate PG gate.
package identity_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	transport "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/httpapi"
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

func TestConfiguredIdentityRouteContract(t *testing.T) {
	// Construct the production transport without a repository. Route registration
	// needs no database; this test inspects registrations without invoking handlers.
	handler := transport.New(nil, "https://admin.example.test", 30*time.Minute)
	server, err := bootstrap.New(bootstrap.Config{Routes: handler.Routes(), Authenticator: handler})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, ok := server.Handler.(interface{ RouteSpecs() []httpapi.RouteSpec })
	if !ok {
		t.Fatal("bootstrap must expose successful registration snapshots")
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileRoutes(doc, snapshot.RouteSpecs()); err != nil {
		t.Fatal(err)
	}
	// Removing any installed business handler must fail the full merged contract.
	for i, route := range snapshot.RouteSpecs() {
		if route.OperationID == "health" {
			continue
		}
		t.Run("missing_"+route.OperationID, func(t *testing.T) {
			specs := snapshot.RouteSpecs()
			specs = append(specs[:i:i], specs[i+1:]...)
			if err := reconcileRoutes(doc, specs); err == nil {
				t.Fatal("missing registered handler accepted")
			}
		})
	}
}
