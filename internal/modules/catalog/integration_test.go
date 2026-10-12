//go:build pgx

// Responsibilities: exercise catalog HTTP and generated production SQL on PG18.
// Dependencies: platform router and pgxadapter public runner; callers: C07 gates.
// Invariants: actual runtime login, tenant isolation, CAS single winner,
// normalized uniqueness, deterministic pagination and atomic option assignment.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func pgRouter(t *testing.T) (*httpapi.Router, *httpapi.Router) {
	t.Helper()
	dsn := os.Getenv("C07_DATABASE_URL")
	if dsn == "" {
		t.Skip("NOT_RUN: C07_DATABASE_URL requires disposable migrated PG18 and catalog seed.sql")
	}
	parsed, err := url.Parse(dsn)
	// The internal app_runtime role stays NOLOGIN. This disposable dedicated
	// login inherits its privileges; root seeds only the acceptance database.
	if err != nil || parsed.User == nil || parsed.User.Username() != "c07_runtime" {
		t.Fatal("C07_DATABASE_URL must login as the dedicated c07_runtime fixture role")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runner, err := pgxadapter.OpenQueryRunner(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runner.Close)
	service := New(Config{Run: runner.Queries})
	return testRouter(t, service, testAuth{tenant: tenantA}), testRouter(t, service, testAuth{tenant: tenantB})
}
func expect(t *testing.T, r http.Handler, method, path, body, version string, status int) *httptest.ResponseRecorder {
	t.Helper()
	rr := httpRequest(r, method, path, body, version)
	if rr.Code != status {
		t.Fatalf("%s %s HTTP%d want%d: %s", method, path, rr.Code, status, rr.Body.String())
	}
	return rr
}
func decode[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(rr.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}
func newProduct(t *testing.T, r http.Handler, tenant string) generated.Product {
	t.Helper()
	code := "p-" + uuid.NewString()
	body, _ := json.Marshal(generated.CreateProduct{ProductCode: " \t" + code + "\r\n"})
	p := decode[generated.Product](t, expect(t, r, "POST", "/api/v1/t/"+tenant+"/products", string(body), "", 201))
	if p.ProductCode != strings.ToUpper(code) || p.Version != 1 || p.LifecycleStatus != "draft" || p.CreatedAt.IsZero() {
		t.Fatalf("created %+v", p)
	}
	return p
}
func TestPGCatalogCRUD(t *testing.T) {
	a, b := pgRouter(t)
	p := newProduct(t, a, tenantA)
	path := "/api/v1/t/" + tenantA + "/products/" + p.Id.String()
	expect(t, a, "POST", "/api/v1/t/"+tenantA+"/products", `{"product_code":"`+strings.ToLower(p.ProductCode)+`"}`, "", 409)
	got := decode[generated.Product](t, expect(t, a, "GET", path, "", "", 200))
	if got.Id != p.Id || got.Version != 1 {
		t.Fatalf("get %+v", got)
	}
	updated := decode[generated.Product](t, expect(t, a, "PATCH", path, `{"lifecycle_status":"ready"}`, `"1"`, 200))
	if updated.Version != 2 || updated.LifecycleStatus != "ready" {
		t.Fatalf("update %+v", updated)
	}
	expect(t, a, "PATCH", path, `{"product_code":"STALE"}`, `"1"`, 412)
	expect(t, a, "DELETE", path, "", `"1"`, 412)
	foreign := newProduct(t, b, tenantB)
	foreignPath := "/api/v1/t/" + tenantA + "/products/" + foreign.Id.String()
	expect(t, a, "GET", foreignPath, "", "", 404)
	expect(t, a, "PATCH", foreignPath, `{"product_code":"X"}`, `"1"`, 404)
	expect(t, a, "DELETE", foreignPath, "", `"1"`, 404)
	variant := decode[generated.Variant](t, expect(t, a, "POST", path+"/variants", `{"sku":" sku-`+uuid.NewString()+` ","option_value_ids":[]}`, "", 201))
	if variant.Version != 1 || variant.LifecycleStatus != "draft" || len(variant.OptionValueIds) != 0 || variant.CreatedAt.IsZero() {
		t.Fatalf("variant %+v", variant)
	}
	vp := path + "/variants/" + variant.Id.String()
	expect(t, a, "GET", vp, "", "", 200)
	list := decode[generated.VariantList](t, expect(t, a, "GET", path+"/variants", "", "", 200))
	if len(list.Items) != 1 || list.Items[0].Id != variant.Id {
		t.Fatalf("list %+v", list)
	}
	renamed := "RENAMED-" + uuid.NewString()
	v := decode[generated.Variant](t, expect(t, a, "PATCH", vp, `{"sku":" `+renamed+` ","lifecycle_status":"ready"}`, `"1"`, 200))
	if v.Version != 2 || v.Sku != strings.ToUpper(renamed) || v.LifecycleStatus != "ready" {
		t.Fatalf("variant updated %+v", v)
	}
	expect(t, a, "PATCH", vp, `{"sku":"STALE"}`, `"1"`, 412)
	expect(t, a, "DELETE", vp, "", `"2"`, 204)
	v = decode[generated.Variant](t, expect(t, a, "GET", vp, "", "", 200))
	if v.Version != 3 || v.LifecycleStatus != "archived" {
		t.Fatalf("variant archived %+v", v)
	}
	expect(t, a, "PATCH", vp, `{"sku":"NO"}`, `"3"`, 409)
	expect(t, a, "DELETE", path, "", `"2"`, 204)
	p = decode[generated.Product](t, expect(t, a, "GET", path, "", "", 200))
	if p.Version != 3 || p.LifecycleStatus != "archived" {
		t.Fatalf("product archived %+v", p)
	}
	expect(t, a, "PATCH", path, `{"product_code":"NO"}`, `"3"`, 409)
	expect(t, a, "POST", path+"/variants", `{"sku":"NO","option_value_ids":[]}`, "", 409)
}
func TestPGCatalogCASOneWinner(t *testing.T) {
	a, _ := pgRouter(t)
	p := newProduct(t, a, tenantA)
	path := "/api/v1/t/" + tenantA + "/products/" + p.Id.String()
	start := make(chan struct{})
	codes := make(chan int, 12)
	var group sync.WaitGroup
	for n := 0; n < 12; n++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			codes <- httpRequest(a, "PATCH", path, `{"lifecycle_status":"ready"}`, `"1"`).Code
		}()
	}
	close(start)
	group.Wait()
	close(codes)
	winners := 0
	for code := range codes {
		if code == 200 {
			winners++
		} else if code != 412 {
			t.Fatalf("CAS unexpected HTTP%d", code)
		}
	}
	if winners != 1 {
		t.Fatalf("CAS winners%d want1", winners)
	}
	p = decode[generated.Product](t, expect(t, a, "GET", path, "", "", 200))
	if p.Version != 2 {
		t.Fatalf("CAS version%d", p.Version)
	}
}
func TestPGCatalogPagination(t *testing.T) {
	a, b := pgRouter(t)
	foreign := newProduct(t, b, tenantB)
	for n := 0; n < 105; n++ {
		newProduct(t, a, tenantA)
	}
	base := "/api/v1/t/" + tenantA + "/products"
	first := decode[generated.ProductPage](t, expect(t, a, "GET", base, "", "", 200))
	if len(first.Items) != 20 || first.NextCursor == nil {
		t.Fatalf("default page %+v", first)
	}
	capped := decode[generated.ProductPage](t, expect(t, a, "GET", base+"?page_size=100", "", "", 200))
	if len(capped.Items) != 100 || capped.NextCursor == nil {
		t.Fatalf("max page count%d", len(capped.Items))
	}
	seen := make(map[uuid.UUID]bool)
	var prev *generated.Product
	next := ""
	pages := 0
	for {
		pages++
		if pages > 100 {
			t.Fatal("cursor did not terminate")
		}
		path := base + "?page_size=17"
		if next != "" {
			path += "&cursor=" + url.QueryEscape(next)
		}
		page := decode[generated.ProductPage](t, expect(t, a, "GET", path, "", "", 200))
		for _, p := range page.Items {
			if seen[p.Id] || p.Id == foreign.Id {
				t.Fatalf("duplicate or foreign %s", p.Id)
			}
			if prev != nil && (p.CreatedAt.After(prev.CreatedAt) || (p.CreatedAt.Equal(prev.CreatedAt) && p.Id.String() >= prev.Id.String())) {
				t.Fatal("cursor ordering violated")
			}
			seen[p.Id] = true
			copy := p
			prev = &copy
		}
		if page.NextCursor == nil {
			break
		}
		next = *page.NextCursor
	}
	if len(seen) < 105 {
		t.Fatalf("pagination lost products: %d", len(seen))
	}
}
func TestPGCatalogOptions(t *testing.T) {
	a, _ := pgRouter(t)
	base := "/api/v1/t/" + tenantA + "/products/33333333-3333-4333-8333-333333333333/variants"
	red := "55555555-5555-4555-8555-555555555551"
	blue := "55555555-5555-4555-8555-555555555552"
	large := "55555555-5555-4555-8555-555555555553"
	foreign := "55555555-5555-4555-8555-555555555554"
	ambiguous := "55555555-5555-4555-8555-555555555556"
	body := func(ids ...string) string {
		b, _ := json.Marshal(map[string]any{"sku": "OPTIONS-" + uuid.NewString(), "option_value_ids": ids})
		return string(b)
	}
	expect(t, a, "POST", base, body(red, blue), "", 422)
	expect(t, a, "POST", base, body(foreign), "", 422)
	expect(t, a, "POST", base, body(missingID), "", 422)
	expect(t, a, "POST", base, body(ambiguous), "", 422)
	v := decode[generated.Variant](t, expect(t, a, "POST", base, body(large, red), "", 201))
	actual := []string{}
	for _, id := range v.OptionValueIds {
		actual = append(actual, id.String())
	}
	sort.Strings(actual)
	if fmt.Sprint(actual) != fmt.Sprint([]string{red, large}) {
		t.Fatalf("option assignment %v", actual)
	}
	expect(t, a, "POST", base, body(red, large), "", 409)
	list := decode[generated.VariantList](t, expect(t, a, "GET", base, "", "", 200))
	if len(list.Items) != 1 || list.Items[0].Id != v.Id {
		t.Fatalf("failed requests persisted rows %+v", list)
	}
	// A variant id from another product must stay hidden under this product.
	p := newProduct(t, a, tenantA)
	expect(t, a, "GET", "/api/v1/t/"+tenantA+"/products/"+p.Id.String()+"/variants/"+v.Id.String(), "", "", 404)
}
