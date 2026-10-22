//go:build pgx

// Responsibility: accept the admin order read API (listOrders/getOrder) end to
// end through production bootstrap, router and the runtime-role snapshot runner.
// Dependencies: orders public commands for committed fixtures, pgxadapter, an
// expendable migrated DATABASE_URL (superuser maintenance for cleanup only).
// Invariants: pagination is stable and filter-bound, money is decimal strings,
// guest contact never leaves, other tenants are 404, the read path cannot write.
package db_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type orderReadAuth struct{ tenant string }

func (a orderReadAuth) Authenticate(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	if spec.Access != httpapi.AdminOrAPIKey || spec.Scope != "orders:read" {
		return nil, apperrors.New(403, "forbidden", "Scope denied")
	}
	if r.PathValue("tenant_id") != a.tenant {
		return nil, apperrors.New(404, "not_found", "Tenant not found")
	}
	return ctx, nil
}

type orderReadFixture struct {
	t                *testing.T
	ctx              context.Context
	maintenance      pgxadapter.Runner
	runtime          *pgxadapter.QueryRunner
	tenant, foreign  string
	guest, customer  string // order ids in tenant
	discounted, paid string
	foreignOrder     string
	unsealed         string
	runtimeDSN       string
}

// create commits one order through the public orders command as app_runtime.
func (f *orderReadFixture) create(tenant, sku string, mutate func(*contract.CreateInput)) string {
	f.t.Helper()
	var id string
	if err := f.maintenance.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT app.inventory_adjust($1,5,'order read receipt',$2,'inbound')", sku, uuid.NewString()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
			return err
		}
		in := orderFixture{tenant: tenant, sku: sku}.input(f.t)
		if mutate != nil {
			mutate(&in)
		}
		created, err := orders.New().Create(ctx, tx, tenant, in)
		id = created.ID
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	// Distinct clock_timestamp() values keep the expected newest-first order exact.
	time.Sleep(2 * time.Millisecond)
	return id
}

func (f *orderReadFixture) scan(sql string, args []any, dest ...any) {
	f.t.Helper()
	if err := f.maintenance.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(dest...)
	}); err != nil {
		f.t.Fatal(err)
	}
}

func newOrderReadFixture(t *testing.T) *orderReadFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: expendable migrated PostgreSQL DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	maintenance, closeMaintenance, err := pgxadapter.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeMaintenance)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("DATABASE_URL must be a URL")
	}
	query := parsed.Query()
	query.Set("application_name", fmt.Sprintf("orders_read_acceptance_%d", time.Now().UnixNano()))
	parsed.RawQuery = query.Encode()
	runtime, err := pgxadapter.OpenQueryRunner(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Close)
	f := &orderReadFixture{t: t, ctx: ctx, maintenance: maintenance, runtime: runtime, runtimeDSN: parsed.String()}
	var product, sku, foreignProduct, foreignSKU, customer string
	f.scan("INSERT INTO app.tenants(name) VALUES('orders-read') RETURNING id::text", nil, &f.tenant)
	f.scan("INSERT INTO app.tenants(name) VALUES('orders-read-foreign') RETURNING id::text", nil, &f.foreign)
	t.Cleanup(func() {
		clean, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		// Order snapshots are immutable by trigger; this expendable-DB cleanup
		// runs as the maintenance superuser with user triggers suspended and is
		// bound to the two unique fixture tenants on every table carrying one.
		if err := maintenance.InMaintenance(clean, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role=replica"); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
				JOIN pg_attribute a ON a.attrelid=c.oid AND a.attname='tenant_id' AND NOT a.attisdropped
				WHERE n.nspname='app' AND c.relkind IN ('r','p') AND NOT c.relispartition`)
			if err != nil {
				return err
			}
			tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			for _, table := range tables {
				if _, err = tx.Exec(ctx, "DELETE FROM app."+pgx.Identifier{table}.Sanitize()+" WHERE tenant_id=ANY($1::uuid[])", []string{f.tenant, f.foreign}); err != nil {
					return err
				}
			}
			_, err = tx.Exec(ctx, "DELETE FROM app.tenants WHERE id=ANY($1::uuid[])", []string{f.tenant, f.foreign})
			return err
		}); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	f.scan("INSERT INTO app.products(tenant_id,product_code) VALUES($1,'ORDREAD-P') RETURNING id::text", []any{f.tenant}, &product)
	f.scan("INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES($1,$2,'ORDER-SKU','default') RETURNING id::text", []any{f.tenant, product}, &sku)
	f.scan("INSERT INTO app.products(tenant_id,product_code) VALUES($1,'ORDREAD-P') RETURNING id::text", []any{f.foreign}, &foreignProduct)
	f.scan("INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES($1,$2,'ORDER-SKU','default') RETURNING id::text", []any{f.foreign, foreignProduct}, &foreignSKU)
	f.scan("INSERT INTO app.customers(tenant_id,email) VALUES($1,'orders-read@example.invalid') RETURNING id::text", []any{f.tenant}, &customer)

	f.guest = f.create(f.tenant, sku, nil)
	f.customer = f.create(f.tenant, sku, func(in *contract.CreateInput) { in.Buyer = contract.Buyer{CustomerID: customer} })
	f.discounted = f.create(f.tenant, sku, func(in *contract.CreateInput) {
		in.Adjustments = []contract.Adjustment{{ID: uuid.NewString(), Kind: "discount", AmountMinor: -20, Reason: "loyalty"}}
	})
	f.paid = f.create(f.tenant, sku, nil)
	if err = maintenance.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", f.tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
			return err
		}
		_, err := orders.New().Transition(ctx, tx, f.tenant, contract.TransitionInput{OrderID: f.paid, Target: contract.Paid, EventKey: "read-test:paid"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.foreignOrder = f.create(f.foreign, foreignSKU, nil)
	// An unsealed (in-construction) snapshot cannot exist via orders_create, so
	// plant one with the guard trigger suspended; reads must never expose it.
	f.unsealed = uuid.NewString()
	if err = maintenance.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role=replica"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,
			items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor,sealed)
			VALUES($1,$2,1000000,'unsealed-read-probe',decode(repeat('ab',32),'hex'),'{"email":"guest@example.test"}','USD',0,0,0,'{}',0,false)`,
			f.tenant, f.unsealed)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *orderReadFixture) handler(read func(context.Context, string, func(*db.Queries) error) error) http.Handler {
	f.t.Helper()
	server, err := bootstrap.New(bootstrap.Config{Authenticator: orderReadAuth{tenant: f.tenant}, Orders: orders.HTTPConfig{Read: read},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		f.t.Fatal(err)
	}
	return server.Handler
}

func orderGet(t *testing.T, h http.Handler, path string, want int) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != want {
		t.Fatalf("GET %s: HTTP %d want %d: %s", path, w.Code, want, w.Body.String())
	}
	if want != 200 && !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("problem content type: %q", w.Header().Get("Content-Type"))
	}
	return w
}

type orderPage struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

func decodePage(t *testing.T, w *httptest.ResponseRecorder) orderPage {
	t.Helper()
	var p orderPage
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOrdersReadAPIPostgres(t *testing.T) {
	f := newOrderReadFixture(t)
	h := f.handler(f.runtime.RunAuthorizedSnapshot)
	base := "/api/v1/t/" + f.tenant + "/orders"

	// Pagination: newest first, stable across pages, no overlap, no guest PII.
	first := orderGet(t, h, base+"?limit=3", 200)
	if first.Header().Get("Cache-Control") != "no-store" || strings.Contains(first.Body.String(), "guest@example.test") {
		t.Fatalf("list headers/PII: %v %s", first.Header(), first.Body.String())
	}
	p1 := decodePage(t, first)
	if len(p1.Items) != 3 || p1.NextCursor == nil {
		t.Fatalf("page 1: %s", first.Body.String())
	}
	p2 := decodePage(t, orderGet(t, h, base+"?limit=3&cursor="+url.QueryEscape(*p1.NextCursor), 200))
	if len(p2.Items) != 1 || p2.NextCursor != nil {
		t.Fatalf("page 2: %+v", p2)
	}
	var got []string
	for _, item := range append(p1.Items, p2.Items...) {
		got = append(got, item["id"].(string))
	}
	want := []string{f.paid, f.discounted, f.customer, f.guest}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order %v want %v", got, want)
	}
	kinds := map[string]string{}
	for _, item := range append(p1.Items, p2.Items...) {
		total, isString := item["total_minor"].(string)
		if !isString || len(item) != 7 {
			t.Fatalf("summary shape/money: %v", item)
		}
		kinds[item["id"].(string)] = item["customer_kind"].(string)
		if item["id"] == f.discounted && total != "250" || item["id"] == f.guest && total != "270" {
			t.Fatalf("total: %v", item)
		}
	}
	if kinds[f.customer] != "customer" || kinds[f.guest] != "guest" {
		t.Fatalf("customer kinds: %v", kinds)
	}
	all := decodePage(t, orderGet(t, h, base, 200))
	if len(all.Items) != 4 || all.NextCursor != nil {
		t.Fatalf("default page (unsealed or foreign rows must not appear): %+v", all)
	}
	orderGet(t, h, base+"/"+f.unsealed, 404)

	// State filter and cursor binding.
	paid := decodePage(t, orderGet(t, h, base+"?state=paid", 200))
	if len(paid.Items) != 1 || paid.Items[0]["id"] != f.paid || paid.Items[0]["state"] != "paid" {
		t.Fatalf("state filter: %+v", paid)
	}
	pending := decodePage(t, orderGet(t, h, base+"?state=pending_payment&limit=1", 200))
	if len(pending.Items) != 1 || pending.Items[0]["id"] != f.discounted || pending.NextCursor == nil {
		t.Fatalf("pending page: %+v", pending)
	}
	pending2 := decodePage(t, orderGet(t, h, base+"?state=pending_payment&limit=5&cursor="+url.QueryEscape(*pending.NextCursor), 200))
	if len(pending2.Items) != 2 || pending2.Items[0]["id"] != f.customer || pending2.Items[1]["id"] != f.guest {
		t.Fatalf("pending page 2: %+v", pending2)
	}
	orderGet(t, h, base+"?limit=3&cursor="+url.QueryEscape(*pending.NextCursor), 400)
	orderGet(t, h, base+"?state=paid&cursor="+url.QueryEscape(*p1.NextCursor), 400)
	// A well-formed cursor forged from another tenant's order position still
	// returns only the caller's rows: it is a seek key, not an authorization.
	var foreignAt time.Time
	f.scan("SELECT created_at FROM app.orders WHERE tenant_id=$1 AND id=$2", []any{f.foreign, f.foreignOrder}, &foreignAt)
	forgedJSON, err := json.Marshal(struct {
		T  time.Time `json:"t"`
		ID string    `json:"id"`
		S  string    `json:"s"`
	}{foreignAt.Add(time.Hour).UTC(), f.foreignOrder, ""})
	if err != nil {
		t.Fatal(err)
	}
	forged := decodePage(t, orderGet(t, h, base+"?limit=100&cursor="+base64.RawURLEncoding.EncodeToString(forgedJSON), 200))
	if len(forged.Items) != 4 {
		t.Fatalf("forged cursor page: %+v", forged)
	}
	for _, item := range forged.Items {
		if id := item["id"]; id != f.guest && id != f.customer && id != f.discounted && id != f.paid {
			t.Fatalf("forged cursor leaked row %v", id)
		}
	}
	// Invalid cursors are rejected deterministically: every witness below is
	// either the real canonical cursor with a byte outside the base64url
	// alphabet, a fixed literal, or another real cursor used under the wrong
	// filter. None can drift into an accidentally valid cursor (see
	// parseOrderCursor, internal/modules/orders/http.go:110-130).
	canonical := *p1.NextCursor
	nonBase64 := []byte(canonical)
	nonBase64[len(nonBase64)/2] = '!' // '!' is never part of base64url.
	canonicalJSON, err := base64.RawURLEncoding.DecodeString(canonical)
	if err != nil {
		t.Fatal(err)
	}
	var canonicalFields struct {
		Time  time.Time `json:"t"`
		ID    uuid.UUID `json:"id"`
		State string    `json:"s"`
	}
	if err := json.Unmarshal(canonicalJSON, &canonicalFields); err != nil {
		t.Fatal(err)
	}
	malformedUUID, err := json.Marshal(struct {
		Time  time.Time `json:"t"`
		ID    string    `json:"id"`
		State string    `json:"s"`
	}{canonicalFields.Time, "not-a-uuid", canonicalFields.State})
	if err != nil {
		t.Fatal(err)
	}
	// (e) A cursor minted for another tenant is not an invalid-cursor witness:
	// the cursor carries no tenant, and the forged foreign-position block above
	// asserts it is honoured as a seek key (200) while returning only the
	// caller's rows. There is no 400 behaviour to pin without changing product
	// code, so (e) is intentionally absent.
	for _, tc := range []struct {
		name   string
		cursor string
		query  string
	}{
		{"non-base64url character", string(nonBase64), ""},
		{"base64url of non-JSON bytes", base64.RawURLEncoding.EncodeToString([]byte("not-json")), ""},
		{"malformed UUID", base64.RawURLEncoding.EncodeToString(malformedUUID), ""},
		{"filter mismatch", *pending.NextCursor, "state=paid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := orderGet(t, h, base+"?limit=3&"+tc.query+"cursor="+url.QueryEscape(tc.cursor), 400)
			var problem struct {
				Code   string `json:"code"`
				Status int    `json:"status"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil || problem.Status != 400 || problem.Code != "invalid_cursor" {
				t.Fatalf("%s: problem %s", tc.name, w.Body.String())
			}
		})
	}
	orderGet(t, h, base+"?state=shipped", 400)

	// Detail shape, ETag, money strings, adjustments and nulls.
	w := orderGet(t, h, base+"/"+f.discounted, 200)
	if w.Header().Get("ETag") != `"1"` || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "guest@example.test") {
		t.Fatalf("detail headers/PII: %v %s", w.Header(), w.Body.String())
	}
	var detail map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	lines := detail["lines"].([]any)
	line := lines[0].(map[string]any)
	adjustment := detail["adjustments"].([]any)[0].(map[string]any)
	shipping := detail["shipping"].(map[string]any)
	if len(detail) != 12 || len(lines) != 1 || detail["version"] != float64(1) || detail["total_minor"] != "250" ||
		detail["tax_total_minor"] != "20" || detail["currency"] != "USD" || detail["state"] != "pending_payment" {
		t.Fatalf("detail: %s", w.Body.String())
	}
	if line["sku_code"] != "ORDER-SKU" || line["title"] != nil || line["quantity"] != float64(2) || line["unit_price_minor"] != "100" ||
		line["tax_minor"] != "20" || line["line_total_minor"] != "220" || len(line) != 7 {
		t.Fatalf("line: %v", line)
	}
	if adjustment["amount_minor"] != "-20" || adjustment["reason"] != "loyalty" || len(adjustment) != 3 {
		t.Fatalf("adjustment: %v", adjustment)
	}
	if shipping["amount_minor"] != "50" || shipping["method"] != "fixed" {
		t.Fatalf("shipping: %v", shipping)
	}
	if paidDetail := orderGet(t, h, base+"/"+f.paid, 200); paidDetail.Header().Get("ETag") != `"2"` ||
		!strings.Contains(paidDetail.Body.String(), `"adjustments":[]`) {
		t.Fatalf("paid detail: %v %s", paidDetail.Header(), paidDetail.Body.String())
	}

	// Cross-tenant and missing orders are indistinguishable 404s.
	orderGet(t, h, base+"/"+f.foreignOrder, 404)
	orderGet(t, h, base+"/"+uuid.NewString(), 404)
	orderGet(t, h, "/api/v1/t/"+f.foreign+"/orders/"+f.foreignOrder, 404)
	orderGet(t, h, "/api/v1/t/"+f.foreign+"/orders", 404)
	for _, item := range all.Items {
		if item["id"] == f.foreignOrder {
			t.Fatal("foreign order listed")
		}
	}

	// The read path itself refuses a writable transaction (fail closed).
	orderGet(t, f.handler(f.runtime.RunAuthorized), base+"/"+f.discounted, 503)

	// Read-only: a write inside the production snapshot transaction is refused
	// by PostgreSQL and leaves the order untouched.
	var writeErr error
	probe := f.handler(func(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
		return f.runtime.RunAuthorizedSnapshot(ctx, tenant, func(q *db.Queries) error {
			_, writeErr = q.TransitionOrder(ctx, db.TransitionOrderParams{ID: mustPGUUID(t, f.discounted), Target: "cancelled", EventKey: "read-probe", RefundMinor: 0})
			return writeErr
		})
	})
	orderGet(t, probe, base+"/"+f.discounted, 503)
	var pgErr *pgxadapter.PgError
	if !errors.As(writeErr, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("write in read path: %v", writeErr)
	}
	var state string
	var version int64
	f.scan("SELECT state,version FROM app.orders WHERE tenant_id=$1 AND id=$2", []any{f.tenant, f.discounted}, &state, &version)
	if state != "pending_payment" || version != 1 {
		t.Fatalf("read path mutated order: %s v%d", state, version)
	}
}

func mustPGUUID(t *testing.T, id string) pgtype.UUID {
	t.Helper()
	parsed, err := uuid.Parse(id)
	if err != nil {
		t.Fatal(err)
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}
}

// Production wiring: FromEnvironment must give the orders read path a
// read-only runner. A writable runner is refused by the read-only guard.
func TestOrdersReadAPIProductionWiringIsReadOnly(t *testing.T) {
	f := newOrderReadFixture(t)
	t.Setenv("OPENSITE_DATABASE_URL", f.runtimeDSN)
	config, closeRuntime, err := bootstrap.FromEnvironment(f.ctx, bootstrap.Config{Authenticator: orderReadAuth{tenant: f.tenant},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeRuntime)
	server, err := bootstrap.New(config)
	if err != nil {
		t.Fatal(err)
	}
	w := orderGet(t, server.Handler, "/api/v1/t/"+f.tenant+"/orders/"+f.paid, 200)
	if w.Header().Get("ETag") != `"2"` {
		t.Fatalf("production wiring detail: %v %s", w.Header(), w.Body.String())
	}
	orderGet(t, server.Handler, "/api/v1/t/"+f.tenant+"/orders?state=paid", 200)
}
