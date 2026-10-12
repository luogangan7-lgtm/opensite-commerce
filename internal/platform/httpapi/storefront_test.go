// Responsibility: exercise anonymous strict transport and host admission.
// Dependencies: router/publish contract; caller: Go gate; invariant: bad query never reads.
package httpapi_test

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

func (r *publishedReader) Current(_ context.Context, host string) (contract.Site, error) {
	r.calls++
	r.host = host
	return contract.Site{SiteID: "s", ReleaseID: "r", Hostname: host, Locale: "en", Title: "Shop", Description: "Text", Navigation: []contract.Navigation{}, Pages: []contract.Page{}, Products: []contract.Product{{ID: "p", Slug: "aid", Name: "Aid", Description: "Text", UnitAmount: "9007199254740993", Currency: "USD"}}}, r.err
}
func TestStorefrontAdmission(t *testing.T) {
	reader := &publishedReader{}
	router := httpapi.New(nil, nil)
	route := httpapi.StorefrontRoute(reader)
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
