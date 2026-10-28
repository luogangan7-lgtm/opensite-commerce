// Responsibility: expose synthetic admission contracts only to this package's tests.
// Dependencies: private Router registration and OpenAPI; caller: external httpapi tests.
// Invariant: this function does not exist in a production build or dependency.
package httpapi

import (
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
)

func RegisterFixtureContract(router *Router, spec RouteSpec, handler http.Handler, contract *openapi3.T) error {
	return router.registerWithContract(spec, handler, contract)
}
