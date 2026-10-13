//go:build pgx

// Responsibility: compare the generated production query with the pure quote contract.
// Dependencies: pgx, an expendable migrated PG18 DATABASE_URL, pricing and money.
// Caller: check_codegen/pgx tests; fixtures roll back and run as scoped app_runtime.
// Invariants: every filtering axis, UUID/tier ordering, and tenant RLS agree.
package db_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
)

func TestPricingGeneratedQuery(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: expendable migrated PostgreSQL DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	uuid := func(s string) pgtype.UUID {
		t.Helper()
		var u pgtype.UUID
		if s != "" {
			if err := u.Scan(s); err != nil {
				t.Fatal(err)
			}
		}
		return u
	}
	seedSKU := func() (tenant, sku string) {
		t.Helper()
		var product string
		if err := tx.QueryRow(ctx, "INSERT INTO app.tenants(name) VALUES('query-gate') RETURNING id::text").Scan(&tenant); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, "INSERT INTO app.products(tenant_id,product_code) VALUES($1,'P') RETURNING id::text", uuid(tenant)).Scan(&product); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, "INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES($1,$2,'SKU','default') RETURNING id::text", uuid(tenant), uuid(product)).Scan(&sku); err != nil {
			t.Fatal(err)
		}
		return
	}
	tenant, sku := seedSKU()
	foreignTenant, foreignSKU := seedSKU()
	id := func(n int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012x", n) }
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := start.Add(24 * time.Hour)
	boundaryStart, boundaryEnd := start.Add(48*time.Hour), start.Add(72*time.Hour)
	expiredStart, expiredEnd := start.Add(-48*time.Hour), start
	lists := []pricing.PriceList{
		{ID: id(1), Priority: 5, Entries: []pricing.Entry{{MinQuantity: 1, UnitAmount: 150}, {MinQuantity: 10, UnitAmount: 120}}},
		{ID: id(2), Priority: 5, Entries: []pricing.Entry{{MinQuantity: 1, UnitAmount: 100}}},
		{ID: id(3), Priority: 6, Audience: "customer_group", CustomerGroupID: id(90), Entries: []pricing.Entry{{MinQuantity: 1, UnitAmount: 70}}},
		{ID: id(4), Priority: 7, Audience: "buyer_company", BuyerCompanyID: id(91), Entries: []pricing.Entry{{MinQuantity: 1, UnitAmount: 60}}},
		{ID: id(5), Priority: 100, Status: "draft", Entries: []pricing.Entry{{MinQuantity: 1, UnitAmount: 1}}},
		{ID: id(6), Priority: 100, Status: "archived", Entries: []pricing.Entry{{MinQuantity: 1, UnitAmount: 1}}},
		{ID: id(7), Priority: 100, Currency: "EUR", Entries: []pricing.Entry{{MinQuantity: 1, UnitAmount: 1}}},
		{ID: id(8), Priority: 100, StartsAt: start.AddDate(0, 1, 0), Entries: []pricing.Entry{{MinQuantity: 1, UnitAmount: 1}}},
		{ID: id(9), Priority: 100, StartsAt: expiredStart, EndsAt: &expiredEnd, Entries: []pricing.Entry{{MinQuantity: 1, UnitAmount: 1}}},
		{ID: id(10), Priority: 99, Entries: []pricing.Entry{{MinQuantity: 1000, UnitAmount: 1}}},
		{ID: id(11), Priority: 9, StartsAt: boundaryStart, EndsAt: &boundaryEnd, Entries: []pricing.Entry{{MinQuantity: 1, UnitAmount: 50}}},
		{ID: id(12), TenantID: foreignTenant, Priority: 200, Entries: []pricing.Entry{{SKUID: foreignSKU, MinQuantity: 1, UnitAmount: 1}}},
	}
	for i := range lists {
		l := &lists[i]
		if l.TenantID == "" {
			l.TenantID = tenant
		}
		if l.Currency == "" {
			l.Currency = "USD"
		}
		if l.Status == "" {
			l.Status = "active"
		}
		if l.Audience == "" {
			l.Audience = "all"
		}
		if l.StartsAt.IsZero() {
			l.StartsAt = start
		}
		// B2B now owns the buyer target referenced by Pricing's composite FK.
		if l.BuyerCompanyID != "" {
			exec("INSERT INTO app.buyer_companies(tenant_id,id,name) VALUES($1,$2,'query buyer')", uuid(l.TenantID), uuid(l.BuyerCompanyID))
		}
		exec("INSERT INTO app.price_lists(tenant_id,id,currency,status,priority,audience,starts_at,ends_at,customer_group_id,buyer_company_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", uuid(l.TenantID), uuid(l.ID), l.Currency, l.Status, l.Priority, l.Audience, l.StartsAt, l.EndsAt, uuid(l.CustomerGroupID), uuid(l.BuyerCompanyID))
		for j := range l.Entries {
			e := &l.Entries[j]
			if e.SKUID == "" {
				e.SKUID = sku
			}
			exec("INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount) VALUES($1,$2,$3,$4,$5)", uuid(l.TenantID), uuid(l.ID), uuid(e.SKUID), e.MinQuantity, e.UnitAmount)
		}
	}
	exec("SELECT set_config('app.tenant_id',$1,true)", tenant)
	exec("SET LOCAL ROLE app_runtime")
	query := db.New(tx)
	cases := []struct {
		name, currency, group, company, sku string
		quantity                            int64
		at                                  time.Time
		rows, selected                      int
		amount, tier                        int64
		err                                 error
	}{
		{name: "tie-tier", quantity: 10, rows: 3, selected: 1, amount: 120, tier: 10},
		{name: "base-tier", quantity: 1, rows: 2, selected: 1, amount: 150, tier: 1},
		{name: "group", group: id(90), quantity: 10, rows: 4, selected: 3, amount: 70, tier: 1},
		{name: "company", company: id(91), quantity: 10, rows: 4, selected: 4, amount: 60, tier: 1},
		{name: "both", group: id(90), company: id(91), quantity: 10, rows: 5, selected: 4, amount: 60, tier: 1},
		{name: "unmatched-audience", group: id(92), company: id(93), quantity: 10, rows: 3, selected: 1, amount: 120, tier: 10},
		{name: "start-inclusive", at: boundaryStart, quantity: 10, rows: 4, selected: 11, amount: 50, tier: 1},
		{name: "end-exclusive", at: boundaryEnd, quantity: 10, rows: 3, selected: 1, amount: 120, tier: 10},
		{name: "other-currency", currency: "EUR", quantity: 10, rows: 1, selected: 7, amount: 1, tier: 1},
		{name: "no-currency", currency: "GBP", quantity: 10, err: pricing.ErrNoPrice},
		{name: "no-truncation", currency: "USDx", quantity: 10, err: money.ErrInvalidCurrency},
		{name: "no-quantity", quantity: 0, err: pricing.ErrInvalid},
		{name: "foreign-sku", sku: foreignSKU, quantity: 10, err: pricing.ErrNoPrice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.currency == "" {
				tc.currency = "USD"
			}
			if tc.at.IsZero() {
				tc.at = at
			}
			if tc.sku == "" {
				tc.sku = sku
			}
			rows, err := query.ListApplicablePriceEntries(ctx, db.ListApplicablePriceEntriesParams{TenantID: uuid(tenant), SkuID: uuid(tc.sku), Currency: tc.currency, Quantity: tc.quantity, AtTime: pgtype.Timestamptz{Time: tc.at, Valid: true}, CustomerGroupID: uuid(tc.group), BuyerCompanyID: uuid(tc.company)})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tc.rows {
				t.Fatalf("production query returned %d rows; want %d", len(rows), tc.rows)
			}
			quote, err := pricing.Quote(lists, pricing.Request{TenantID: tenant, SKUID: tc.sku, Currency: tc.currency, Quantity: tc.quantity, At: tc.at, CustomerGroupID: tc.group, BuyerCompanyID: tc.company})
			if !errors.Is(err, tc.err) {
				t.Fatalf("Quote error %v; want %v", err, tc.err)
			}
			if tc.rows == 0 {
				return
			}
			row := rows[0]
			if row.PriceListID != uuid(id(tc.selected)) || row.UnitAmount != tc.amount || row.MinQuantity != tc.tier || row.Currency != tc.currency {
				t.Fatalf("unexpected production winner: %+v", row)
			}
			if quote.PriceListID != id(tc.selected) || quote.Unit.Amount != row.UnitAmount || quote.Line.Amount != tc.amount*tc.quantity || quote.MinQuantity != row.MinQuantity || quote.Priority != row.Priority || quote.Unit.Currency != row.Currency || quote.Line.Currency != row.Currency {
				t.Fatalf("Go/SQL disagree: quote %+v, row %+v", quote, row)
			}
		})
	}
	// Query parameters cannot grant authority beyond the transaction's tenant.
	rows, err := query.ListApplicablePriceEntries(ctx, db.ListApplicablePriceEntriesParams{TenantID: uuid(foreignTenant), SkuID: uuid(foreignSKU), Currency: "USD", Quantity: 1, AtTime: pgtype.Timestamptz{Time: at, Valid: true}})
	if err != nil || len(rows) != 0 {
		t.Fatalf("foreign tenant query: %d rows, %v", len(rows), err)
	}
	rows, err = query.ListApplicablePriceEntries(ctx, db.ListApplicablePriceEntriesParams{TenantID: uuid(foreignTenant), SkuID: uuid(sku), Currency: "USD", Quantity: 10, AtTime: pgtype.Timestamptz{Time: at, Valid: true}})
	if err != nil || len(rows) != 0 {
		t.Fatalf("tenant parameter/context mismatch: %d rows, %v", len(rows), err)
	}
	exec("SELECT set_config('app.tenant_id','',true)")
	rows, err = query.ListApplicablePriceEntries(ctx, db.ListApplicablePriceEntriesParams{TenantID: uuid(tenant), SkuID: uuid(sku), Currency: "USD", Quantity: 10, AtTime: pgtype.Timestamptz{Time: at, Valid: true}})
	if err != nil || len(rows) != 0 {
		t.Fatalf("blank tenant query: %d rows, %v", len(rows), err)
	}
}
