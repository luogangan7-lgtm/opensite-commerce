// Responsibility: guard process-wide embedded contract reuse under concurrent reads.
// Dependencies: httpapi and kin-openapi; caller: the CI race suite.
// Invariant: every caller receives the same resolved, read-only document.
package httpapi_test

import (
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func TestAPIContractParsedOnce(t *testing.T) {
	const callers = 4
	docs := make([]*openapi3.T, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			docs[i], errs[i] = httpapi.GetSwagger()
		}()
	}
	close(start)
	wg.Wait()
	for i := range callers {
		if errs[i] != nil || docs[i] == nil {
			t.Fatalf("load %d: %v", i, errs[i])
		}
		if docs[i] != docs[0] {
			t.Fatal("API contract parsed more than once per process")
		}
	}
}
