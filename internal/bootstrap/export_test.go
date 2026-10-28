// Responsibility: admit explicitly declared synthetic routes only in tests.
// Dependencies: static server composition and HTTP fixture admission contracts.
// Invariant: all non-fixture routes still use the embedded production contract.
package bootstrap

import (
	"net/http"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/httpapitest"
)

func NewTestServer(t testing.TB, config Config) (*http.Server, error) {
	t.Helper()
	fixtures := make(map[string]bool)
	for _, route := range config.Routes {
		fixtures[route.Spec.OperationID] = true
	}
	return newServer(config, func(router *httpapi.Router, route httpapi.Route) error {
		if fixtures[route.Spec.OperationID] {
			return httpapitest.Register(t, router, false, route.Spec, route.Handler)
		}
		return router.Register(route.Spec, route.Handler)
	})
}
