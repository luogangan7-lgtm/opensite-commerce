// Responsibility: exercise concurrent server composition against the shared OAS.
// Dependencies: bootstrap, httpapi and httptest; caller: the CI race suite.
// Invariant: startup and request validation never mutate the published document.
package bootstrap_test

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func TestConcurrentBootstrapPreservesAPIContract(t *testing.T) {
	doc, err := httpapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			server, err := bootstrap.New(bootstrap.Config{Authenticator: runtimeActorAuth{}})
			if err != nil {
				t.Error(err)
				return
			}
			w := httptest.NewRecorder()
			server.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
			if w.Code != 204 {
				t.Errorf("health status = %d", w.Code)
			}
			const products = "/api/v1/t/11111111-1111-1111-1111-111111111111/products"
			for _, tc := range []struct{ method, path, body string }{
				{"POST", products, `{"product_code":"p","unexpected":true}`},
				{"GET", products + "?page_size=101", ""},
			} {
				req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				server.Handler.ServeHTTP(w, req)
				if w.Code != 400 {
					t.Errorf("schema admission %s %s = %d: %s", tc.method, tc.path, w.Code, w.Body.String())
				}
			}
			shared, err := httpapi.GetSwagger()
			if err != nil || shared != doc {
				t.Errorf("bootstrap did not share API contract: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	after, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("server composition mutated the shared API contract")
	}
}
