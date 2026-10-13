//go:build pgx

// Responsibility: exercise quote public ports against migrated PostgreSQL18.
// Dependencies: pgxadapter owns the only pool; test-only SQL seeds expendable fixtures.
// Invariants: production SQL is generated, runtime is non-superuser; no missing-DB skip.
package quotes_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	q "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type quoteFixture struct {
	tenant, site, staff, company, buyer, approver, foreignCompany, profile, inquiry string
	runner                                                                          pgxadapter.Runner
	ctx                                                                             context.Context
}

func quotesDB(t *testing.T) quoteFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Fatal("NOT_RUN: mandatory expendable migrated PostgreSQL DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	r, close, e := pgxadapter.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(close)
	f := quoteFixture{tenant: uuid.NewString(), site: uuid.NewString(), staff: uuid.NewString(), company: uuid.NewString(), buyer: uuid.NewString(), approver: uuid.NewString(), foreignCompany: uuid.NewString(), profile: uuid.NewString(), inquiry: uuid.NewString(), runner: r, ctx: ctx}
	org, zone, taxzone := uuid.NewString(), uuid.NewString(), uuid.NewString()
	e = r.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		steps := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO app.tenants(id,name,settlement_currency) VALUES($1,'quotes-go-gate','USD')`, []any{f.tenant}},
			{`INSERT INTO app.users(id,email,password_hash) VALUES($1,$2,'fixture-hash')`, []any{f.staff, f.staff + "@example.test"}},
			{`INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES($1,$2,'admin')`, []any{f.tenant, f.staff}},
			{`INSERT INTO app.organizations(tenant_id,id,display_name) VALUES($1,$2,'quotes-go')`, []any{f.tenant, org}},
			{`INSERT INTO app.sites(tenant_id,id,organization_id,name) VALUES($1,$2,$3,'quotes-go')`, []any{f.tenant, f.site, org}},
			{`INSERT INTO app.buyer_companies(tenant_id,id,name) VALUES($1,$2,'quotes-company'),($1,$3,'other-company')`, []any{f.tenant, f.company, f.foreignCompany}},
			{`INSERT INTO app.customers(tenant_id,id,email) VALUES($1,$2,$4),($1,$3,$5)`, []any{f.tenant, f.buyer, f.approver, f.buyer + "@example.test", f.approver + "@example.test"}},
			{`INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code) VALUES($1,$2,$3,'admin'),($1,$2,$4,'approver'),($1,$5,$3,'purchaser')`, []any{f.tenant, f.company, f.buyer, f.approver, f.foreignCompany}},
			{`INSERT INTO app.inquiries(tenant_id,site_id,id,buyer_email,message,buyer_company_id) VALUES($1,$2,$3,'buyer@example.test','quote please',$4)`, []any{f.tenant, f.site, f.inquiry, f.company}},
			{`INSERT INTO app.tax_settings(tenant_id,prices_include_tax) VALUES($1,false)`, []any{f.tenant}},
			{`INSERT INTO app.tax_zones(tenant_id,id,name) VALUES($1,$2,'US')`, []any{f.tenant, taxzone}},
			{`INSERT INTO app.tax_zone_locations(tenant_id,zone_id,country_code) VALUES($1,$2,'US')`, []any{f.tenant, taxzone}},
			{`INSERT INTO app.tax_categories(tenant_id,code,name) VALUES($1,'standard','Standard')`, []any{f.tenant}},
			{`INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from) VALUES($1,$2,'standard',1000,clock_timestamp()-interval '1 hour')`, []any{f.tenant, taxzone}},
			{`INSERT INTO app.shipping_zones(tenant_id,id,name) VALUES($1,$2,'US')`, []any{f.tenant, zone}},
			{`INSERT INTO app.shipping_zone_destinations(tenant_id,zone_id,country_code) VALUES($1,$2,'US')`, []any{f.tenant, zone}},
			{`INSERT INTO app.shipping_profiles(tenant_id,id,name,currency) VALUES($1,$2,'Standard','USD')`, []any{f.tenant, f.profile}},
			{`INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount) VALUES($1,$2,$3,'fixed','USD',50)`, []any{f.tenant, f.profile, zone}},
		}
		for i, s := range steps {
			if _, e := tx.Exec(ctx, s.sql, s.args...); e != nil {
				return fmt.Errorf("fixture %d: %w", i, e)
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func (f quoteFixture) actor(t *testing.T, kind, id, company string) c.AuthorizedActor {
	t.Helper()
	i := c.ActorIdentity{TenantID: f.tenant}
	if kind == "staff" {
		i.StaffID = id
	} else {
		i.CustomerID = id
		i.CompanyID = company
	}
	a, e := c.ActorFromContext(c.WithTrustedActor(f.ctx, i))
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func (f quoteFixture) run(fn func(context.Context, pgx.Tx) error) error {
	return f.runner.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); e != nil {
			return e
		}
		return fn(ctx, tx)
	})
}
func (f quoteFixture) input() c.CreateInput {
	return c.CreateInput{SiteID: f.site, InquiryID: f.inquiry, CompanyID: f.company, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour), Lines: []c.LineInput{{Description: "negotiated price", Quantity: 2, UnitPrice: 100, TaxCategory: "standard"}}, Destination: tax.Destination{Country: "US"}, ShippingProfileID: f.profile}
}
func TestQuotesPGLifecycleReplayAndAuthority(t *testing.T) {
	f := quotesDB(t)
	m := q.New()
	staff := f.actor(t, "staff", f.staff, "")
	buyer := f.actor(t, "customer", f.buyer, f.company)
	approver := f.actor(t, "customer", f.approver, f.company)
	in := f.input()
	opts := c.WriteOptions{IfMatch: "*", IdempotencyKey: uuid.NewString()}
	var draft c.Quote
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		var e error
		draft, e = m.Create(ctx, tx, staff, in, opts)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if draft.ExpiresAt.Location() != time.UTC {
		t.Fatalf("fresh quote expiry is not canonical UTC: %v", draft.ExpiresAt.Location())
	}
	if draft.Status != "draft" || draft.Version != 1 || draft.Snapshot.Total != 270 {
		t.Fatalf("draft %+v", draft)
	}
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		got, e := m.Create(ctx, tx, staff, in, opts)
		if e == nil && got.ExpiresAt.Location() != time.UTC {
			t.Fatalf("replayed quote expiry is not canonical UTC: %v", got.ExpiresAt.Location())
		}
		if e == nil && !reflect.DeepEqual(got, draft) {
			t.Fatalf("replay changed: %+v", got)
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
	changed := in
	changed.Lines = append([]c.LineInput(nil), in.Lines...)
	changed.Lines[0].UnitPrice = 101
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error { _, e := m.Create(ctx, tx, staff, changed, opts); return e }); !errors.Is(e, c.ErrIdempotencyReused) {
		t.Fatalf("hash mismatch %v", e)
	}
	current := draft
	transition := func(actor c.AuthorizedActor, action c.Action) error {
		return f.run(func(ctx context.Context, tx pgx.Tx) error {
			out, e := m.Transition(ctx, tx, actor, current.ID, action, c.WriteOptions{IfMatch: current.ETag(), IdempotencyKey: uuid.NewString()})
			if e == nil {
				current = out
			}
			return e
		})
	}
	if e := transition(staff, c.Send); e != nil {
		t.Fatal(e)
	}
	if e := transition(buyer, c.Accept); e != nil {
		t.Fatal(e)
	}
	if e := transition(buyer, c.Approve); !errors.Is(e, c.ErrForbidden) {
		t.Fatalf("self approval %v", e)
	}
	if e := transition(approver, c.Approve); e != nil {
		t.Fatal(e)
	}
	if current.Status != "approved" || current.Version != 4 || !reflect.DeepEqual(current.Snapshot, draft.Snapshot) {
		t.Fatalf("immutable lifecycle %+v", current)
	}
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		_, e := m.GetAcceptedQuote(ctx, tx, buyer, current.ID)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	// A valid trusted identity scoped to a different tenant cannot distinguish a
	// foreign quote UUID from a missing quote; the tenant runner enforces RLS.
	otherTenant := uuid.NewString()
	foreign, err := c.ActorFromContext(c.WithTrustedActor(f.ctx, c.ActorIdentity{TenantID: otherTenant, CustomerID: f.buyer, CompanyID: f.company}))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.runner.InTenant(f.ctx, otherTenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
			return err
		}
		_, err := m.Get(ctx, tx, foreign, current.ID)
		return err
	}); !errors.Is(err, c.ErrNotFound) {
		t.Fatalf("cross tenant quote: %v", err)
	}
	wrongCompany := f.actor(t, "customer", f.buyer, f.foreignCompany)
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error { _, e := m.Get(ctx, tx, wrongCompany, current.ID); return e }); !errors.Is(e, c.ErrNotFound) {
		t.Fatalf("other company access %v", e)
	}
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		_, e := m.Transition(ctx, tx, staff, current.ID, c.Withdraw, c.WriteOptions{IfMatch: draft.ETag(), IdempotencyKey: uuid.NewString()})
		return e
	}); !errors.Is(e, c.ErrPrecondition) {
		t.Fatalf("stale version %v", e)
	}
	// A completed receipt cannot survive a revoked employee's authority check.
	if e := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE app.tenant_memberships SET status='suspended' WHERE tenant_id=$1 AND user_id=$2`, f.tenant, f.staff)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error { _, e := m.Create(ctx, tx, staff, in, opts); return e }); !errors.Is(e, c.ErrForbidden) {
		t.Fatalf("revoked replay %v", e)
	}
}
func TestQuotesPGExpiryAndPublicInquiry(t *testing.T) {
	f := quotesDB(t)
	m := q.New()
	staff := f.actor(t, "staff", f.staff, "")
	buyer := f.actor(t, "customer", f.buyer, f.company)
	var draft c.Quote
	in := f.input()
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		var e error
		draft, e = m.Create(ctx, tx, staff, in, c.WriteOptions{IfMatch: "*", IdempotencyKey: uuid.NewString()})
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		_, e := m.GetAcceptedQuote(ctx, tx, buyer, draft.ID)
		return e
	}); !errors.Is(e, c.ErrNotFound) {
		t.Fatalf("draft checkout read %v", e)
	}
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		out, e := m.Transition(ctx, tx, staff, draft.ID, c.Send, c.WriteOptions{IfMatch: draft.ETag(), IdempotencyKey: uuid.NewString()})
		if e == nil {
			draft = out
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
	expired := q.NewWithClock(func() time.Time { return in.ExpiresAt })
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		_, e := expired.Transition(ctx, tx, buyer, draft.ID, c.Accept, c.WriteOptions{IfMatch: draft.ETag(), IdempotencyKey: uuid.NewString()})
		return e
	}); !errors.Is(e, c.ErrConflict) {
		t.Fatalf("expiry %v", e)
	}
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		receipt, e := m.SubmitInquiry(ctx, tx, f.tenant, c.InquiryInput{SiteID: f.site, Email: "new@example.test", Name: "Buyer", Message: "Please quote"})
		if e == nil && (!c.ValidID(receipt.ID) || receipt.Status != "received") {
			t.Fatalf("inquiry %+v", receipt)
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
}
func TestQuotesPGBuyerCannotSeeDraftAndAcceptanceUsesDBClock(t *testing.T) {
	f := quotesDB(t)
	m := q.New()
	staff := f.actor(t, "staff", f.staff, "")
	buyer := f.actor(t, "customer", f.buyer, f.company)
	approver := f.actor(t, "customer", f.approver, f.company)
	var current c.Quote
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		var e error
		current, e = m.Create(ctx, tx, staff, f.input(), c.WriteOptions{IfMatch: "*", IdempotencyKey: uuid.NewString()})
		return e
	}); e != nil {
		t.Fatal(e)
	}
	// Draft terms are seller-internal: the buyer cannot read or act on them.
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error { _, e := m.Get(ctx, tx, buyer, current.ID); return e }); !errors.Is(e, c.ErrNotFound) {
		t.Fatalf("buyer read draft: %v", e)
	}
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		_, e := m.Transition(ctx, tx, buyer, current.ID, c.Accept, c.WriteOptions{IfMatch: current.ETag(), IdempotencyKey: uuid.NewString()})
		return e
	}); !errors.Is(e, c.ErrNotFound) {
		t.Fatalf("buyer accept draft: %v", e)
	}
	for _, step := range []struct {
		actor  c.AuthorizedActor
		action c.Action
	}{{staff, c.Send}, {buyer, c.Accept}, {approver, c.Approve}} {
		if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
			out, e := m.Transition(ctx, tx, step.actor, current.ID, step.action, c.WriteOptions{IfMatch: current.ETag(), IdempotencyKey: uuid.NewString()})
			current = out
			return e
		}); e != nil {
			t.Fatalf("%s: %v", step.action, e)
		}
	}
	// The DB clock, not a skewed API host clock, decides whether an approved quote is still valid.
	future := q.NewWithClock(func() time.Time { return time.Now().Add(48 * time.Hour) })
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		_, e := future.GetAcceptedQuote(ctx, tx, buyer, current.ID)
		return e
	}); e != nil {
		t.Fatalf("fast host clock rejected DB-valid quote: %v", e)
	}
	if e := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range []string{`ALTER TABLE app.quotes DISABLE TRIGGER quote_guard`, `UPDATE app.quotes SET expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND id=$2`, `ALTER TABLE app.quotes ENABLE TRIGGER quote_guard`} {
			args := []any{}
			if strings.HasPrefix(s, "UPDATE") {
				args = []any{f.tenant, current.ID}
			}
			if _, e := tx.Exec(ctx, s, args...); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	past := q.NewWithClock(func() time.Time { return time.Unix(0, 0) })
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		_, e := past.GetAcceptedQuote(ctx, tx, buyer, current.ID)
		return e
	}); !errors.Is(e, c.ErrConflict) {
		t.Fatalf("slow host clock accepted DB-expired quote: %v", e)
	}
}
func TestQuotesPGHundredLineQuotesPersistAndReplay(t *testing.T) {
	f := quotesDB(t)
	m := q.New()
	staff := f.actor(t, "staff", f.staff, "")
	// Short lines, multi-byte maximal descriptions, and maximal JSON-escaped descriptions.
	for _, description := range []string{"short", strings.Repeat("é", 1000), strings.Repeat("\x01", 1000)} {
		in := f.input()
		in.Lines = nil
		for range c.MaxQuoteLines {
			in.Lines = append(in.Lines, c.LineInput{Description: description, Quantity: 2, UnitPrice: 100, TaxCategory: "standard"})
		}
		opts := c.WriteOptions{IfMatch: "*", IdempotencyKey: uuid.NewString()}
		var first c.Quote
		for i := range 2 {
			if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
				got, e := m.Create(ctx, tx, staff, in, opts)
				if e == nil && i == 1 && !reflect.DeepEqual(got, first) {
					t.Fatalf("100-line replay changed")
				}
				first = got
				return e
			}); e != nil {
				t.Fatalf("100 lines of %d-byte descriptions (attempt %d): %v", len(description), i, e)
			}
		}
		if len(first.Snapshot.Lines) != c.MaxQuoteLines || first.Snapshot.Subtotal != 20000 {
			t.Fatalf("100-line snapshot %d %d", len(first.Snapshot.Lines), first.Snapshot.Subtotal)
		}
	}
}
func TestQuotesPGUnrelatedProfileZoneDoesNotShadow(t *testing.T) {
	f := quotesDB(t)
	staff := f.actor(t, "staff", f.staff, "")
	// Another profile ships to a more specific California zone; the quote's
	// profile only has the country-wide US zone (ADR-C13a profile-relevant zones).
	other, ca := uuid.NewString(), uuid.NewString()
	if e := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO app.shipping_zones(tenant_id,id,name) VALUES($1,$2,'California')`, []any{f.tenant, ca}},
			{`INSERT INTO app.shipping_zone_destinations(tenant_id,zone_id,country_code,subdivision_code) VALUES($1,$2,'US','US-CA')`, []any{f.tenant, ca}},
			{`INSERT INTO app.shipping_profiles(tenant_id,id,name,currency) VALUES($1,$2,'Freight','USD')`, []any{f.tenant, other}},
			{`INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount) VALUES($1,$2,$3,'fixed','USD',999)`, []any{f.tenant, other, ca}},
		} {
			if _, e := tx.Exec(ctx, s.sql, s.args...); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	in := f.input()
	in.Destination = tax.Destination{Country: "US", Subdivision: "US-CA"}
	var got c.Quote
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		var e error
		got, e = q.New().Create(ctx, tx, staff, in, c.WriteOptions{IfMatch: "*", IdempotencyKey: uuid.NewString()})
		return e
	}); e != nil {
		t.Fatalf("unrelated profile zone shadowed the quote profile: %v", e)
	}
	if got.Snapshot.ShippingAmount != 50 {
		t.Fatalf("shipping %d, want profile rate 50", got.Snapshot.ShippingAmount)
	}
}

// approveBetweenReads commits a concurrent approval just before the second
// statement of the caller's READ COMMITTED transaction.
type approveBetweenReads struct {
	pgx.Tx
	calls  int
	before func()
}

func (h *approveBetweenReads) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	h.calls++
	if h.calls == 2 {
		h.before()
	}
	return h.Tx.QueryRow(ctx, sql, args...)
}
func TestQuotesPGAcceptedQuoteIsTheRowThatPassedChecks(t *testing.T) {
	f := quotesDB(t)
	m := q.New()
	staff := f.actor(t, "staff", f.staff, "")
	buyer := f.actor(t, "customer", f.buyer, f.company)
	approver := f.actor(t, "customer", f.approver, f.company)
	var current c.Quote
	if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		var e error
		current, e = m.Create(ctx, tx, staff, f.input(), c.WriteOptions{IfMatch: "*", IdempotencyKey: uuid.NewString()})
		return e
	}); e != nil {
		t.Fatal(e)
	}
	for _, step := range []struct {
		actor  c.AuthorizedActor
		action c.Action
	}{{staff, c.Send}, {buyer, c.Accept}} {
		if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
			out, e := m.Transition(ctx, tx, step.actor, current.ID, step.action, c.WriteOptions{IfMatch: current.ETag(), IdempotencyKey: uuid.NewString()})
			current = out
			return e
		}); e != nil {
			t.Fatal(e)
		}
	}
	pending := current
	var got c.Quote
	e := f.run(func(ctx context.Context, tx pgx.Tx) error {
		h := &approveBetweenReads{Tx: tx, before: func() {
			if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
				_, e := m.Transition(ctx, tx, approver, pending.ID, c.Approve, c.WriteOptions{IfMatch: pending.ETag(), IdempotencyKey: uuid.NewString()})
				return e
			}); e != nil {
				t.Errorf("concurrent approve: %v", e)
			}
		}}
		var e error
		got, e = m.GetAcceptedQuote(ctx, h, buyer, pending.ID)
		return e
	})
	// Either refusal or the approved row is safe; a stale pending row is not.
	if e == nil && (got.Status != "approved" || got.Version != pending.Version+1) {
		t.Fatalf("returned stale %s v%d after checks passed", got.Status, got.Version)
	}
	if e != nil && !errors.Is(e, c.ErrConflict) {
		t.Fatal(e)
	}
}
