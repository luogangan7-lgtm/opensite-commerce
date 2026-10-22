//go:build pgx

// Responsibilities: verify production price HTTP and sqlc against real PG18.
// Dependencies: disposable CI admin and C07 runtime logins; caller: pgx CI gate.
// Invariants: explicit settlement currency, persisted prices and list-wide CAS.
package bootstrap_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/pricing"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestPGStaffPricingPersistence(t *testing.T) {
	dsn := os.Getenv("C07_DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "C07_DATABASE_URL requires disposable migrated PG18")
	}
	ctx := context.Background()
	root, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close(ctx)
	tenant, product, variant := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, q := range []string{
		fmt.Sprintf("INSERT INTO app.tenants(id,name) VALUES('%s','staff-price-http')", tenant),
		fmt.Sprintf("INSERT INTO app.products(tenant_id,id,product_code) VALUES('%s','%s','HTTP')", tenant, product),
		fmt.Sprintf("INSERT INTO app.product_variants(tenant_id,product_id,id,sku,option_key) VALUES('%s','%s','%s','HTTP','')", tenant, product, variant),
	} {
		if _, err := root.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, table := range []string{"price_entries", "price_lists", "product_variants", "products", "tenants"} {
			column := "tenant_id"
			if table == "tenants" {
				column = "id"
			}
			if _, err := root.Exec(ctx, "DELETE FROM app."+table+" WHERE "+column+"=$1", tenant); err != nil {
				t.Error(err)
			}
		}
	}()
	runner, err := pgxadapter.OpenQueryRunner(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	server, err := bootstrap.New(bootstrap.Config{Authenticator: pricingAuth("owner"), Pricing: pricing.Config{Run: runner.RunAuthorized}})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body, version string, want int) map[string]any {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("If-Match", version)
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("STAFF_PRICE_PG %s: %d want %d: %s", method, w.Code, want, w.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	base := "/api/v1/t/" + tenant + "/price-lists"
	body := `{"currency":"USD","status":"active","starts_at":"2026-01-01T00:00:00Z","priority":0,"audience":"all"}`
	if request("POST", base, body, "", 409)["code"] != "settlement_currency_required" {
		t.Fatal("missing currency was defaulted")
	}
	if _, err := root.Exec(ctx, "UPDATE app.tenants SET settlement_currency='USD' WHERE id=$1", tenant); err != nil {
		t.Fatal(err)
	}
	list := request("POST", base, body, "", 201)
	path := base + "/" + list["id"].(string)
	tier := path + "/variants/" + variant + "/prices/1"
	request("PUT", tier, `{"unit_amount":125}`, `"1"`, 200)
	request("PUT", tier, `{"unit_amount":300}`, `"1"`, 412)
	if request("GET", path, "", "", 200)["version"] != float64(2) {
		t.Fatal("list CAS not persisted")
	}
	var amount int64
	if err := root.QueryRow(ctx, "SELECT unit_amount FROM app.price_entries WHERE tenant_id=$1", tenant).Scan(&amount); err != nil || amount != 125 {
		t.Fatalf("price persistence %d %v", amount, err)
	}
	request("DELETE", tier, "", `"2"`, 200)
}
