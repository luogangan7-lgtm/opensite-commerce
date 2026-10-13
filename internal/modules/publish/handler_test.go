// Responsibility: exercise anonymous strict transport and host admission.
// Dependencies: router/publish contract; caller: Go gate; invariant: bad query never reads.
package publish

import (
	"context"
	"encoding/json"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"net/http"
	"net/http/httptest"
	"testing"
)

type publishedReader struct {
	calls int
	host  string
	err   error
}

// servedBody is deliberately spaced: the transport must write stored bytes as-is.
const servedBody = `{"tenant_id":"t", "site_id":"s","release_id":"r","locale":"en","title":"Shop","description":"Text","navigation":[],"pages":[],"products":[{"id":"p","slug":"aid","name":"Aid","description":"Text","unit_amount":"9007199254740993","currency":"USD","skus":[]}]}` + "\n"

func (r *publishedReader) Serve(_ context.Context, host string) ([]byte, error) {
	r.calls++
	r.host = host
	if r.err != nil {
		return nil, r.err
	}
	return []byte(servedBody), nil
}
func TestStorefrontAdmission(t *testing.T) {
	reader := &publishedReader{}
	router := httpapi.New(nil, nil)
	route := StorefrontRoute(reader)
	if err := router.Register(route.Spec, route.Handler); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"", "host=shop.example:443", "host=shop.example&host=other.example", "host=shop.example&tenant_id=x", "host=%ZZ"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/public/sites/current?"+query, nil))
		if w.Code != 400 || reader.calls != 0 {
			t.Fatalf("%s: %d reads=%d", query, w.Code, reader.calls)
		}
	}
	req := httptest.NewRequest("GET", "/api/v1/public/sites/current?host=SHOP.EXAMPLE.", nil)
	req.Header.Set("X-Forwarded-Host", "private.example")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 || reader.host != "shop.example" {
		t.Fatalf("public read %d %s", w.Code, w.Body)
	}
	if w.Body.String() != servedBody || w.Header().Get("ETag") == "" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("serving bytes not written as-is: %q %v", w.Body.String(), w.Header())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	products := result["products"].([]any)
	if products[0].(map[string]any)["unit_amount"] != "9007199254740993" {
		t.Fatal("amount lost precision")
	}
	reader.err = contract.ErrNotFound
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown host: %d", w.Code)
	}
	reader.err = contract.ErrUnavailable
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 503 {
		t.Fatalf("unavailable: %d", w.Code)
	}
}
