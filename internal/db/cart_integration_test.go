//go:build pgx

// Responsibility: independently accept cart HTTP, actor isolation and live money projections.
// Dependencies: production cart/bootstrap, pgxadapter runners and expendable migrated DATABASE_URL.
// Caller: pgx acceptance gate; unique fixture tenants are cleaned in FK order.
// Invariants: supplied DSN never skips; rejected writes roll back; cart never reserves stock.
package db_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type cartFixture struct {
	t                                                                               *testing.T
	ctx                                                                             context.Context
	db                                                                              pgxadapter.Runner
	runtime                                                                         *pgxadapter.QueryRunner
	appName                                                                         string
	tenant, otherTenant, product, sku, otherSKU, customer, otherCustomer, priceList string
	actor                                                                           contract.AuthorizedActor
	guest, registered                                                               http.Handler
}

func newCartFixture(t *testing.T) *cartFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: expendable migrated PostgreSQL DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	// Fixtures use the maintenance runner; cart gets the production runner on a
	// distinct application_name so lock probes see only cart transactions.
	fixtures, closeFixtures, err := pgxadapter.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeFixtures)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("DATABASE_URL must be a URL")
	}
	appName := fmt.Sprintf("cart_acceptance_%d", time.Now().UnixNano())
	query := parsed.Query()
	query.Set("application_name", appName)
	parsed.RawQuery = query.Encode()
	runtime, err := pgxadapter.OpenQueryRunner(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Close)
	f := &cartFixture{t: t, ctx: ctx, db: fixtures, runtime: runtime, appName: appName}
	f.tenant = f.id("INSERT INTO app.tenants(name) VALUES('cart-acceptance') RETURNING id::text")
	f.otherTenant = f.id("INSERT INTO app.tenants(name) VALUES('cart-acceptance-foreign') RETURNING id::text")
	t.Cleanup(func() {
		cleanCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		// Fixed table names; bound unique tenant IDs cannot touch another fixture.
		for _, table := range []string{"cart_lines", "carts", "price_entries", "price_lists", "stock_levels", "warehouses", "product_variants", "products", "customers", "tenants"} {
			column := "tenant_id"
			if table == "tenants" {
				column = "id"
			}
			if err := fixtures.InMaintenance(cleanCtx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, "DELETE FROM app."+table+" WHERE "+column+"=$1::uuid OR "+column+"=$2::uuid", f.tenant, f.otherTenant)
				return err
			}); err != nil {
				t.Errorf("cleanup %s: %v", table, err)
			}
		}
	})
	f.product = f.id("INSERT INTO app.products(tenant_id,product_code,lifecycle_status) VALUES($1,'CART-P','ready') RETURNING id::text", f.tenant)
	f.sku = f.id("INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key,lifecycle_status) VALUES($1,$2,'CART-SKU','one','ready') RETURNING id::text", f.tenant, f.product)
	f.otherSKU = f.id("INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key,lifecycle_status) VALUES($1,$2,'CART-SKU-2','two','ready') RETURNING id::text", f.tenant, f.product)
	f.customer = f.id("INSERT INTO app.customers(tenant_id,email) VALUES($1,'cart-owner@example.invalid') RETURNING id::text", f.tenant)
	f.otherCustomer = f.id("INSERT INTO app.customers(tenant_id,email) VALUES($1,'cart-other@example.invalid') RETURNING id::text", f.tenant)
	f.priceList = f.id("INSERT INTO app.price_lists(tenant_id,currency,status,priority,audience,starts_at) VALUES($1,'USD','active',1,'all','2020-01-01') RETURNING id::text", f.tenant)
	f.exec("INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount) VALUES($1,$2,$3,1,150),($1,$2,$3,10,120),($1,$2,$4,1,200)", f.tenant, f.priceList, f.sku, f.otherSKU)
	warehouse := f.id("INSERT INTO app.warehouses(tenant_id) VALUES($1) RETURNING id::text", f.tenant)
	f.exec("INSERT INTO app.stock_levels(tenant_id,warehouse_id,sku_id,on_hand,reserved) VALUES($1,$2,$3,7,2),($1,$2,$4,7,0)", f.tenant, warehouse, f.sku, f.otherSKU)
	f.actor = contract.AuthorizedActor{TenantID: f.tenant, CustomerID: f.customer}
	f.guest = f.handler(nil)
	f.registered = f.handler(func(context.Context, *http.Request) (contract.AuthorizedActor, error) { return f.actor, nil })
	return f
}

func (f *cartFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if err := f.db.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
}
func (f *cartFixture) scan(sql string, args []any, dest ...any) {
	f.t.Helper()
	if err := f.db.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(dest...)
	}); err != nil {
		f.t.Fatal(err)
	}
}
func (f *cartFixture) id(sql string, args ...any) string {
	f.t.Helper()
	var id string
	f.scan(sql, args, &id)
	return id
}
func (f *cartFixture) handler(resolver cart.ActorResolver) http.Handler {
	f.t.Helper()
	server, err := bootstrap.New(bootstrap.Config{Cart: cart.NewPostgres(cart.Config{Run: f.runtime.RunAuthorized, Read: f.runtime.RunAuthorizedSnapshot}, resolver), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		f.t.Fatal(err)
	}
	return server.Handler
}

func cartRequest(h http.Handler, method, path, body, token, match string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("X-Cart-Token", token)
	}
	if match != "" {
		r.Header.Set("If-Match", match)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func wantStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP %d; want %d: %s", w.Code, status, w.Body.String())
	}
}
func cartView(t *testing.T, w *httptest.ResponseRecorder) contract.View {
	t.Helper()
	var v contract.View
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func (f *cartFixture) create(h http.Handler) (contract.Created, string) {
	f.t.Helper()
	w := cartRequest(h, "POST", "/api/v1/t/"+f.tenant+"/carts", `{"currency":"USD"}`, "", "")
	wantStatus(f.t, w, 201)
	var c contract.Created
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		f.t.Fatal(err)
	}
	return c, w.Header().Get("ETag")
}
func (f *cartFixture) path(id string) string { return "/api/v1/t/" + f.tenant + "/carts/" + id }
func (f *cartFixture) state(id string) (int64, int64) {
	f.t.Helper()
	var version, quantity int64
	f.scan("SELECT c.version,coalesce((SELECT sum(quantity) FROM app.cart_lines WHERE tenant_id=c.tenant_id AND cart_id=c.id),0)::bigint FROM app.carts c WHERE c.tenant_id=$1 AND c.id=$2", []any{f.tenant, id}, &version, &quantity)
	return version, quantity
}

func TestCartPostgresGuestOwnership(t *testing.T) {
	f := newCartFixture(t)
	c, etag := f.create(f.guest)
	if len(c.CartToken) != 43 || etag == "" || c.Cart.Version != 1 {
		t.Fatalf("invalid created cart: %+v / %s", c, etag)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(c.CartToken)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("invalid canonical token: %v", err)
	}
	hash := sha256.Sum256([]byte(c.CartToken))
	var stored []byte
	var owner *string
	f.scan("SELECT guest_token_hash,customer_id::text FROM app.carts WHERE tenant_id=$1 AND id=$2", []any{f.tenant, c.Cart.ID}, &stored, &owner)
	if !bytes.Equal(stored, hash[:]) || owner != nil || bytes.Equal(stored, []byte(c.CartToken)) {
		t.Fatal("guest cart must persist only token SHA256")
	}
	path := f.path(c.Cart.ID)
	for _, token := range []string{"", strings.Repeat("A", 43), "malformed"} {
		wantStatus(t, cartRequest(f.guest, "GET", path, "", token, ""), 404)
	}
	w := cartRequest(f.guest, "GET", path, "", c.CartToken, "")
	wantStatus(t, w, 200)
	if strings.Contains(w.Body.String(), "cart_token") || strings.Contains(w.Body.String(), c.CartToken) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("guest read leaked capability or cacheable representation")
	}
	w = cartRequest(f.guest, "GET", strings.Replace(path, f.tenant, f.otherTenant, 1), "", c.CartToken, "")
	wantStatus(t, w, 404)
	w = cartRequest(f.registered, "GET", path, "", c.CartToken, "")
	wantStatus(t, w, 404)
	// A credential-less actor cannot inherit staff/tenant header authority.
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("X-Tenant-ID", f.tenant)
	r.Header.Set("Authorization", "Bearer staff-is-not-a-customer")
	w = httptest.NewRecorder()
	f.guest.ServeHTTP(w, r)
	wantStatus(t, w, 404)
}

func TestCartPostgresSuspendedTenant(t *testing.T) {
	f := newCartFixture(t)
	guest, tag := f.create(f.guest)
	registered, registeredTag := f.create(f.registered)
	f.exec("UPDATE app.tenants SET status='suspended' WHERE id=$1", f.tenant)
	for _, tc := range []struct {
		h   http.Handler
		c   contract.Created
		tag string
	}{{f.guest, guest, tag}, {f.registered, registered, registeredTag}} {
		path := f.path(tc.c.Cart.ID)
		wantStatus(t, cartRequest(tc.h, "GET", path, "", tc.c.CartToken, ""), 404)
		wantStatus(t, cartRequest(tc.h, "PUT", path+"/lines/"+f.sku, `{"quantity":1}`, tc.c.CartToken, tc.tag), 404)
		wantStatus(t, cartRequest(tc.h, "POST", "/api/v1/t/"+f.tenant+"/carts", `{"currency":"USD"}`, "", ""), 404)
	}
}

func TestCartPostgresExpiredQuotesRemainRemovable(t *testing.T) {
	f := newCartFixture(t)
	created, tag := f.create(f.registered)
	path := f.path(created.Cart.ID)
	for _, sku := range []string{f.sku, f.otherSKU} {
		w := cartRequest(f.registered, "PUT", path+"/lines/"+sku, `{"quantity":1}`, "", tag)
		wantStatus(t, w, 200)
		tag = w.Header().Get("ETag")
	}
	f.exec("UPDATE app.price_lists SET status='archived' WHERE tenant_id=$1 AND id=$2", f.tenant, f.priceList)
	w := cartRequest(f.registered, "GET", path, "", "", "")
	wantStatus(t, w, 200)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, exists := body["subtotal"]; exists || body["pricing_error"] != "price_unavailable" {
		t.Fatalf("fabricated subtotal: %s", w.Body.String())
	}
	for _, raw := range body["lines"].([]any) {
		line := raw.(map[string]any)
		if _, exists := line["unit_amount"]; exists || line["pricing_error"] != "price_unavailable" {
			t.Fatalf("fabricated line quote: %v", line)
		}
	}
	if w.Header().Get("ETag") == tag {
		t.Fatal("expired quotes kept old ETag")
	}
	for _, sku := range []string{f.sku, f.otherSKU} {
		tag = w.Header().Get("ETag")
		w = cartRequest(f.registered, "DELETE", path+"/lines/"+sku, "", "", tag)
		wantStatus(t, w, 200)
	}
	if v := cartView(t, w); len(v.Lines) != 0 || v.PricingError != "" || v.Subtotal != 0 {
		t.Fatalf("removal did not recover: %+v", v)
	}
}

func TestCartPostgresHTTPIntentAndPricing(t *testing.T) {
	f := newCartFixture(t)
	c, etag := f.create(f.registered)
	if c.CartToken != "" {
		t.Fatal("customer creation returned guest capability")
	}
	path := f.path(c.Cart.ID)
	linePath := path + "/lines/" + f.sku
	w := cartRequest(f.registered, "PUT", linePath, `{"quantity":2}`, "", "")
	wantStatus(t, w, 428)
	wantStatus(t, cartRequest(f.registered, "DELETE", linePath, "", "", ""), 428)
	for _, match := range []string{"*", "W/" + etag, etag + ", " + etag} {
		wantStatus(t, cartRequest(f.registered, "PUT", linePath, `{"quantity":2}`, "", match), 412)
	}
	for _, body := range []string{`null`, `{}`, `{"quantity":null}`, `{"quantity":2,"quantity":3}`, `{"quantity":-1}`, `{"quantity":1000001}`, `{"quantity":1.5}`, `{"quantity":1,"customer_id":"forged"}`} {
		w = cartRequest(f.registered, "PUT", linePath, body, "", etag)
		if w.Code < 400 || w.Code >= 500 {
			t.Fatalf("invalid body %s admitted: %d %s", body, w.Code, w.Body.String())
		}
		if v, q := f.state(c.Cart.ID); v != 1 || q != 0 {
			t.Fatalf("invalid input persisted v%d q%d", v, q)
		}
	}
	w = cartRequest(f.registered, "PUT", linePath, `{"quantity":2}`, "", etag)
	wantStatus(t, w, 200)
	v := cartView(t, w)
	if v.Subtotal != 300 || len(v.Lines) != 1 || v.Lines[0].AvailableQuantity != 5 || !v.Lines[0].Sellable || v.Version != 2 {
		t.Fatalf("wrong live projection: %+v", v)
	}
	w = cartRequest(f.registered, "PUT", linePath, `{"quantity":3}`, "", etag)
	wantStatus(t, w, 412)
	etag = contract.ETag(v)
	w = cartRequest(f.registered, "PUT", linePath, `{"quantity":2}`, "", etag)
	wantStatus(t, w, 200)
	v = cartView(t, w)
	if len(v.Lines) != 1 || v.Lines[0].Quantity != 2 || v.Subtotal != 300 {
		t.Fatal("absolute PUT accumulated quantity")
	}
	etag = w.Header().Get("ETag")
	w = cartRequest(f.registered, "PUT", linePath, `{"quantity":10}`, "", etag)
	wantStatus(t, w, 200)
	v = cartView(t, w)
	if v.Subtotal != 1200 || v.Lines[0].UnitAmount != 120 || v.Lines[0].MinQuantity != 10 || v.Lines[0].Sellable {
		t.Fatalf("tier/availability wrong: %+v", v)
	}
	old := w.Header().Get("ETag")
	version := v.Version
	f.exec("UPDATE app.price_entries SET unit_amount=110 WHERE tenant_id=$1 AND price_list_id=$2 AND sku_id=$3 AND min_quantity=10", f.tenant, f.priceList, f.sku)
	w = cartRequest(f.registered, "GET", path, "", "", "")
	wantStatus(t, w, 200)
	v = cartView(t, w)
	if v.Subtotal != 1100 || v.Version != version || w.Header().Get("ETag") == old {
		t.Fatalf("price change must alter representation ETag without intent: %+v", v)
	}
	w = cartRequest(f.registered, "PUT", linePath, `{"quantity":11}`, "", old)
	wantStatus(t, w, 412)
	old = contract.ETag(v)
	f.exec("UPDATE app.stock_levels SET on_hand=20 WHERE tenant_id=$1 AND sku_id=$2", f.tenant, f.sku)
	w = cartRequest(f.registered, "GET", path, "", "", "")
	wantStatus(t, w, 200)
	v = cartView(t, w)
	if v.Version != version || v.Lines[0].AvailableQuantity != 18 || !v.Lines[0].Sellable || w.Header().Get("ETag") == old {
		t.Fatal("stock change must update live hint and strong ETag")
	}
	etag = contract.ETag(v)
	w = cartRequest(f.registered, "PUT", linePath, `{"quantity":0}`, "", etag)
	wantStatus(t, w, 200)
	v = cartView(t, w)
	if len(v.Lines) != 0 || v.Subtotal != 0 {
		t.Fatal("PUT zero failed to remove line")
	}
	w = cartRequest(f.registered, "PUT", linePath, `{"quantity":1000000}`, "", w.Header().Get("ETag"))
	wantStatus(t, w, 200)
	w = cartRequest(f.registered, "DELETE", linePath, "", "", w.Header().Get("ETag"))
	wantStatus(t, w, 200)
	if len(cartView(t, w).Lines) != 0 {
		t.Fatal("DELETE retained line")
	}
	var reservations, movements, reserved int64
	f.scan("SELECT (SELECT count(*) FROM app.inventory_reservations WHERE tenant_id=$1),(SELECT count(*) FROM app.inventory_movements WHERE tenant_id=$1),(SELECT sum(reserved)::bigint FROM app.stock_levels WHERE tenant_id=$1)", []any{f.tenant}, &reservations, &movements, &reserved)
	if reservations != 0 || movements != 0 || reserved != 2 {
		t.Fatal("cart intent changed inventory")
	}
}

func TestCartPostgresCustomerIsolationAndInvalidSKU(t *testing.T) {
	f := newCartFixture(t)
	c, etag := f.create(f.registered)
	path := f.path(c.Cart.ID)
	foreign := f.handler(func(context.Context, *http.Request) (contract.AuthorizedActor, error) {
		return contract.AuthorizedActor{TenantID: f.tenant, CustomerID: f.otherCustomer}, nil
	})
	w := cartRequest(foreign, "GET", path, "", "", "")
	wantStatus(t, w, 404)
	w = cartRequest(foreign, "PUT", path+"/lines/"+f.sku, `{"quantity":1}`, "", etag)
	wantStatus(t, w, 404)
	missing := "00000000-0000-0000-0000-000000000001"
	w = cartRequest(f.registered, "PUT", path+"/lines/"+missing, `{"quantity":1}`, "", etag)
	if w.Code < 400 || w.Code >= 500 {
		t.Fatalf("missing SKU: %d %s", w.Code, w.Body.String())
	}
	f.exec("UPDATE app.products SET lifecycle_status='archived' WHERE tenant_id=$1 AND id=$2", f.tenant, f.product)
	w = cartRequest(f.registered, "PUT", path+"/lines/"+f.sku, `{"quantity":1}`, "", etag)
	if w.Code < 400 || w.Code >= 500 {
		t.Fatalf("inactive product: %d %s", w.Code, w.Body.String())
	}
	if version, q := f.state(c.Cart.ID); version != 1 || q != 0 {
		t.Fatal("rejected SKU write persisted")
	}
	f.exec("UPDATE app.customers SET status='disabled' WHERE tenant_id=$1 AND id=$2", f.tenant, f.customer)
	w = cartRequest(f.registered, "GET", path, "", "", "")
	wantStatus(t, w, 404)
}

func TestCartPostgresMoneyRollback(t *testing.T) {
	f := newCartFixture(t)
	c, etag := f.create(f.registered)
	path := f.path(c.Cart.ID)
	f.exec("DELETE FROM app.price_entries WHERE tenant_id=$1 AND min_quantity>1", f.tenant)
	f.exec("UPDATE app.price_entries SET unit_amount=$2 WHERE tenant_id=$1 AND sku_id=$3", f.tenant, int64(math.MaxInt64), f.sku)
	w := cartRequest(f.registered, "PUT", path+"/lines/"+f.sku, `{"quantity":2}`, "", etag)
	wantStatus(t, w, 422)
	if v, q := f.state(c.Cart.ID); v != 1 || q != 0 {
		t.Fatal("multiplication overflow persisted intent")
	}
	f.exec("UPDATE app.price_entries SET unit_amount=$2 WHERE tenant_id=$1", f.tenant, int64(math.MaxInt64/2+1))
	w = cartRequest(f.registered, "PUT", path+"/lines/"+f.sku, `{"quantity":1}`, "", etag)
	wantStatus(t, w, 200)
	etag = w.Header().Get("ETag")
	w = cartRequest(f.registered, "PUT", path+"/lines/"+f.otherSKU, `{"quantity":1}`, "", etag)
	wantStatus(t, w, 422)
	if v, q := f.state(c.Cart.ID); v != 2 || q != 1 {
		t.Fatal("subtotal overflow persisted intent")
	}
	f.exec("DELETE FROM app.price_entries WHERE tenant_id=$1 AND sku_id=$2", f.tenant, f.otherSKU)
	w = cartRequest(f.registered, "PUT", path+"/lines/"+f.otherSKU, `{"quantity":1}`, "", etag)
	wantStatus(t, w, 422)
	if v, q := f.state(c.Cart.ID); v != 2 || q != 1 {
		t.Fatal("no-price write persisted intent")
	}
}

func TestCartPostgresConcurrentIfMatch(t *testing.T) {
	f := newCartFixture(t)
	c, etag := f.create(f.registered)
	path := f.path(c.Cart.ID) + "/lines/" + f.sku
	// The blocker holds the cart row lock in its own maintenance transaction
	// until released; it commits, as the original raw transaction did.
	locked, release, blocked := make(chan error, 1), make(chan struct{}), make(chan error, 1)
	go func() {
		blocked <- f.db.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "SELECT id FROM app.carts WHERE tenant_id=$1 AND id=$2 FOR UPDATE", f.tenant, c.Cart.ID)
			locked <- err
			if err != nil {
				return err
			}
			<-release
			return nil
		})
	}()
	if err := <-locked; err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var wg sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 2)
	for _, q := range []int{2, 3} {
		wg.Add(1)
		go func(quantity int) {
			defer wg.Done()
			responses <- cartRequest(f.registered, "PUT", path, fmt.Sprintf(`{"quantity":%d}`, quantity), "", etag)
		}(q)
	}
	// No timing sleeps: prove both production transactions reached blocked SQL.
	waitCtx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	for {
		var waiters int
		if err := f.db.InMaintenance(waitCtx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name=$1 AND wait_event_type='Lock' AND query LIKE '%app.carts%' AND query LIKE '%FOR UPDATE%'", f.appName).Scan(&waiters)
		}); err != nil {
			unblock()
			wg.Wait()
			t.Fatal(err)
		}
		if waiters >= 2 {
			break
		}
		if waitCtx.Err() != nil {
			unblock()
			wg.Wait()
			t.Fatal("both cart mutations did not reach row-lock barrier")
		}
	}
	unblock()
	if err := <-blocked; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(responses)
	success, stale := 0, 0
	for w := range responses {
		switch w.Code {
		case 200:
			success++
		case 412:
			stale++
		default:
			t.Fatalf("concurrent result %d: %s", w.Code, w.Body.String())
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("winners=%d stale=%d", success, stale)
	}
	if version, q := f.state(c.Cart.ID); version != 2 || q < 2 || q > 3 {
		t.Fatalf("race persisted version%d quantity%d", version, q)
	}
}

// A full cart rejects a new SKU before commit and stays readable/repairable;
// the bounded representation fits the platform's buffered response.
func TestCartPostgresLineLimit(t *testing.T) {
	f := newCartFixture(t)
	c, _ := f.create(f.registered)
	f.exec(`WITH v AS (INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 SELECT $1,$2,'CART-FULL-'||g,'full-'||g FROM generate_series(1,$4::int) g RETURNING id)
INSERT INTO app.cart_lines(tenant_id,cart_id,sku_id,quantity) SELECT $1,$3,id,1 FROM v`, f.tenant, f.product, c.Cart.ID, contract.MaxLines-1)
	f.exec("INSERT INTO app.cart_lines(tenant_id,cart_id,sku_id,quantity) VALUES($1,$2,$3,1)", f.tenant, c.Cart.ID, f.sku)
	f.exec(`INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount)
 SELECT $1,$2,id,1,9223372036 FROM app.product_variants WHERE tenant_id=$1 AND sku LIKE 'CART-FULL-%'`, f.tenant, f.priceList)
	path := f.path(c.Cart.ID)
	w := cartRequest(f.registered, "GET", path, "", "", "")
	wantStatus(t, w, 200)
	if n := len(cartView(t, w).Lines); n != contract.MaxLines || w.Body.Len() > 1<<20 {
		t.Fatalf("full cart: %d lines, %d bytes", n, w.Body.Len())
	}
	tag := w.Header().Get("ETag")
	w = cartRequest(f.registered, "PUT", path+"/lines/"+f.otherSKU, `{"quantity":1}`, "", tag)
	wantStatus(t, w, 422)
	if !strings.Contains(w.Body.String(), "cart_line_limit") {
		t.Fatalf("limit code: %s", w.Body.String())
	}
	if version, q := f.state(c.Cart.ID); version != 1 || q != int64(contract.MaxLines) {
		t.Fatalf("rejected line persisted: version%d quantity%d", version, q)
	}
	w = cartRequest(f.registered, "DELETE", path+"/lines/"+f.sku, "", "", tag)
	wantStatus(t, w, 200)
	w = cartRequest(f.registered, "PUT", path+"/lines/"+f.otherSKU, `{"quantity":1}`, "", w.Header().Get("ETag"))
	wantStatus(t, w, 200)
}

func TestCartVariantLifecycle(t *testing.T) {
	f := newCartFixture(t)
	// Existing fixtures predate the variant lifecycle guard; this test makes the
	// independently owned catalog lifecycle explicit before cart admission.
	f.exec("UPDATE app.product_variants SET lifecycle_status='ready' WHERE tenant_id=$1", f.tenant)
	created, match := f.create(f.registered)
	path := f.path(created.Cart.ID)
	added := cartRequest(f.registered, "PUT", path+"/lines/"+f.sku, `{"quantity":2}`, "", match)
	wantStatus(t, added, 200)
	for _, status := range []string{"archived", "draft"} {
		f.exec("UPDATE app.product_variants SET lifecycle_status=$2 WHERE id=$1", f.sku, status)
		read := cartRequest(f.registered, "GET", path, "", "", "")
		wantStatus(t, read, 200)
		view := cartView(t, read)
		if len(view.Lines) != 1 || view.Lines[0].Sellable {
			t.Fatalf("%s variant shown sellable: %+v", status, view)
		}
		rejected := cartRequest(f.registered, "PUT", path+"/lines/"+f.sku, `{"quantity":3}`, "", read.Header().Get("ETag"))
		wantStatus(t, rejected, 422)
		version, quantity := f.state(created.Cart.ID)
		if version != 2 || quantity != 2 {
			t.Fatalf("inactive mutation persisted version=%d quantity=%d", version, quantity)
		}
	}
	removed := cartRequest(f.registered, "PUT", path+"/lines/"+f.sku, `{"quantity":0}`, "", cartRequest(f.registered, "GET", path, "", "", "").Header().Get("ETag"))
	wantStatus(t, removed, 200)
}

func TestCartSnapshotReadOnlyAndStable(t *testing.T) {
	f := newCartFixture(t)
	created, _ := f.create(f.registered)
	cartID := pgtype.UUID{}
	if err := cartID.Scan(created.Cart.ID); err != nil {
		t.Fatal(err)
	}
	tenantID := pgtype.UUID{}
	if err := tenantID.Scan(f.tenant); err != nil {
		t.Fatal(err)
	}
	params := db.GetCartParams{TenantID: tenantID, ID: cartID}
	var writeError error
	read := func(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
		return f.runtime.RunAuthorizedSnapshot(ctx, tenant, func(q *db.Queries) error {
			if err := fn(q); err != nil {
				return err
			}
			_, writeError = q.AdvanceCartVersion(ctx, db.AdvanceCartVersionParams{TenantID: tenantID, ID: cartID, ExpectedVersion: 1})
			return writeError
		})
	}
	handler := func(read func(context.Context, string, func(*db.Queries) error) error) http.Handler {
		server, err := bootstrap.New(bootstrap.Config{Cart: cart.NewPostgres(cart.Config{Run: f.runtime.RunAuthorized, Read: read}, func(context.Context, *http.Request) (contract.AuthorizedActor, error) { return f.actor, nil }), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		return server.Handler
	}
	rejected := cartRequest(handler(read), "GET", f.path(created.Cart.ID), "", "", "")
	wantStatus(t, rejected, 500)
	var pgerr *pgconn.PgError
	if !errors.As(writeError, &pgerr) || pgerr.Code != "25006" {
		t.Fatalf("snapshot allowed write: %v", writeError)
	}
	if version, _ := f.state(created.Cart.ID); version != 1 {
		t.Fatalf("snapshot write persisted version=%d", version)
	}
	var observed int64
	read = func(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
		return f.runtime.RunAuthorizedSnapshot(ctx, tenant, func(q *db.Queries) error {
			if err := fn(q); err != nil {
				return err
			}
			// A separate committed maintenance transaction changes intent after the
			// first snapshot read, without sleep ordering or raw connection access.
			f.exec("UPDATE app.carts SET version=version+1 WHERE id=$1", created.Cart.ID)
			row, err := q.GetCart(ctx, params)
			observed = row.Version
			return err
		})
	}
	stable := cartRequest(handler(read), "GET", f.path(created.Cart.ID), "", "", "")
	wantStatus(t, stable, 200)
	if observed != 1 || cartView(t, stable).Version != 1 {
		t.Fatalf("GET observed mixed snapshot version=%d", observed)
	}
	if version, _ := f.state(created.Cart.ID); version != 2 {
		t.Fatalf("writer did not commit version=%d", version)
	}
}

func TestCartInactiveVariantCannotBeAdded(t *testing.T) {
	f := newCartFixture(t)
	created, match := f.create(f.registered)
	for _, status := range []string{"draft", "archived"} {
		f.exec("UPDATE app.product_variants SET lifecycle_status=$2 WHERE id=$1", f.sku, status)
		rejected := cartRequest(f.registered, "PUT", f.path(created.Cart.ID)+"/lines/"+f.sku, `{"quantity":1}`, "", match)
		wantStatus(t, rejected, 422)
		if version, quantity := f.state(created.Cart.ID); version != 1 || quantity != 0 {
			t.Fatalf("%s admission persisted %d/%d", status, version, quantity)
		}
	}
}

func TestCartAnonymousCreateRateLimitDoesNotPersist(t *testing.T) {
	f := newCartFixture(t)
	server, err := bootstrap.New(bootstrap.Config{Cart: cart.NewPostgres(cart.Config{Run: f.runtime.RunAuthorized, Read: f.runtime.RunAuthorizedSnapshot, AnonymousCreateLimit: cart.AnonymousCreateLimit{IPBurst: 1}}, nil), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	f.create(server.Handler)
	denied := cartRequest(server.Handler, "POST", "/api/v1/t/"+f.tenant+"/carts", `{"currency":"USD"}`, "", "")
	wantStatus(t, denied, 429)
	if !strings.Contains(denied.Body.String(), "cart_create_rate_limited") || denied.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("bad quota problem: %s", denied.Body.String())
	}
	var count int
	f.scan("SELECT count(*) FROM app.carts WHERE tenant_id=$1", []any{f.tenant}, &count)
	if count != 1 {
		t.Fatalf("quota denial inserted cart count=%d", count)
	}
}
