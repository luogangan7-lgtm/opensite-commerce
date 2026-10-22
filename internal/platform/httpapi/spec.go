// Responsibility: publish the embedded, resolved API contract once per process.
// Dependencies: the pinned generated loader and sync; callers: platform transports.
// Invariant: document and load error are immutable after publication. Callers must
// not mutate the document or run mutating loaders/validators on it after load.
package httpapi

import (
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
)

var apiContract = cacheAPIContract(generated.GetSwagger)

func cacheAPIContract(load func() (*openapi3.T, error)) func() (*openapi3.T, error) {
	return func() (*openapi3.T, error) { return sync.OnceValues(load)() }
}

// GetSwagger returns the shared read-only API contract, including resolved refs.
// Route authorization checks remain in StrictRoutes on every composition, so
// invalid registrations keep their existing startup errors. Load errors are
// returned unchanged; the embedded bytes cannot change during this process.
func GetSwagger() (*openapi3.T, error) { return apiContract() }
