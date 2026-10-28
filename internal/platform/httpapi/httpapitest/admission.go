// Package httpapitest declares synthetic route admission contracts for tests.
// Dependencies: Router and OpenAPI; imported only by test files.
// Invariant: fixture registration retains validation, auth and UUID admission;
// production Register never falls back to this fixture contract.
package httpapitest

import (
	"net/http"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func Register(t testing.TB, router *httpapi.Router, requiresKey bool, spec httpapi.RouteSpec, handler http.Handler) error {
	t.Helper()
	op := &openapi3.Operation{OperationID: spec.OperationID}
	if requiresKey {
		op.Parameters = openapi3.Parameters{{Value: &openapi3.Parameter{Name: "Idempotency-Key", In: "header", Required: true}}}
	}
	item := &openapi3.PathItem{}
	item.SetOperation(spec.Method, op)
	contract := &openapi3.T{Paths: openapi3.NewPaths()}
	contract.Paths.Set(spec.Pattern, item)
	return router.RegisterWithContract(spec, handler, contract)
}
