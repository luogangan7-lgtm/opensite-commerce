// Responsibility: verify one-time contract loading and unchanged failure results.
// Dependencies: the private loader seam and stdlib; caller: the CI race suite.
// Invariant: concurrent callers observe one load, including when loading fails.
package httpapi

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestAPIContractLoaderOnce(t *testing.T) {
	for _, fails := range []bool{false, true} {
		name := "success"
		if fails {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			want := &openapi3.T{}
			var wantErr error
			if fails {
				want = nil
				wantErr = errors.New("invalid embedded API contract")
			}
			var calls atomic.Int32
			load := cacheAPIContract(func() (*openapi3.T, error) {
				calls.Add(1)
				return want, wantErr
			})
			var wg sync.WaitGroup
			start := make(chan struct{})
			for range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					for range 2 {
						got, err := load()
						if got != want || err != wantErr {
							t.Errorf("loader result = (%p, %v), want (%p, %v)", got, err, want, wantErr)
						}
					}
				}()
			}
			close(start)
			wg.Wait()
			if got := calls.Load(); got != 1 {
				t.Fatalf("contract loader called %d times, want 1", got)
			}
		})
	}
}
