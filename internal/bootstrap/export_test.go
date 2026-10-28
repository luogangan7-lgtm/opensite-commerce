// Responsibility: substitute fixture handlers at existing contract paths in tests.
// Dependencies: static server composition and production HTTP registration.
// Invariant: only test builds can replace a composed handler; admission is unchanged.
package bootstrap

import (
	"net/http"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func NewTestServer(t testing.TB, config Config) (*http.Server, error) {
	t.Helper()
	fixtures := make(map[string]bool)
	overridden := make(map[string]bool)
	for _, route := range config.Routes {
		fixtures[route.Spec.OperationID] = true
		overridden[route.Spec.Method+" "+route.Spec.Pattern] = true
	}
	return newServer(config, func(router *httpapi.Router, route httpapi.Route) error {
		if !fixtures[route.Spec.OperationID] && overridden[route.Spec.Method+" "+route.Spec.Pattern] {
			return nil
		}
		return router.Register(route.Spec, route.Handler)
	})
}
