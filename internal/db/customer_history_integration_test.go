//go:build pgx

// Responsibility: exercise customer ownership in production SQL/HTTP under runtime RLS.
// Dependencies: existing disposable order fixture, sqlc, tenant snapshot runner;
// CI is the caller. Invariant: mailbox equality cannot substitute for verified links.
package db_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type historyReadAuth struct{ tenant, customer string }

func (a historyReadAuth) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	return c.WithActor(ctx, c.Actor{TenantID: a.tenant, CustomerID: a.customer, SessionID: uuid.NewString()}), nil
}
func TestCustomerHistorySQLIsolation(t *testing.T) {
	f := newOrderReadFixture(t)
	// The shared fixture uses a maintenance DSN. Pin the read pools to the real
	// runtime role rather than accidentally exercising ownership as a superuser.
	scoped, err := url.Parse(f.runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	params := scoped.Query()
	params.Set("role", "app_runtime")
	scoped.RawQuery = params.Encode()
	role, closeRole, err := pgxadapter.Open(f.ctx, scoped.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeRole)
	var current string
	if err = role.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT current_user").Scan(&current)
	}); err != nil || current != "app_runtime" {
		t.Fatalf("runtime role: %q %v", current, err)
	}
	read, err := pgxadapter.OpenQueryRunner(f.ctx, scoped.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(read.Close)
	var owner, email string
	f.scan("SELECT customer_id::text FROM app.orders WHERE tenant_id=$1 AND id=$2", []any{f.tenant, f.customer}, &owner)
	f.scan("SELECT guest_contact->>'email' FROM app.orders WHERE tenant_id=$1 AND id=$2", []any{f.tenant, f.guest}, &email)
	var changed string
	f.scan("UPDATE app.customers SET email=$2 WHERE tenant_id=$1 AND id=$3 RETURNING id::text", []any{f.tenant, email, owner}, &changed)
	handler := func(customer string) http.Handler {
		r := httpapi.New(historyReadAuth{f.tenant, customer}, nil)
		for _, route := range orders.NewHTTP(orders.HTTPConfig{Read: read.RunAuthorizedSnapshot}).Routes() {
			if err := r.Register(route.Spec, route.Handler); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	base := "/api/v1/t/" + f.tenant + "/customer/orders"
	h := handler(owner)
	other := handler(uuid.NewString())
	if p := decodePage(t, orderGet(t, other, base, 200)); len(p.Items) != 0 {
		t.Fatalf("CUSTOMER_CROSS_LIST_RED: %+v", p)
	}
	orderGet(t, other, base+"/"+f.customer, 404)
	page := decodePage(t, orderGet(t, h, base, 200))
	if len(page.Items) != 1 || page.Items[0]["id"] != f.customer || len(page.Items[0]) != 6 {
		t.Fatalf("CUSTOMER_OWNERSHIP_RED: %+v", page)
	}
	for _, id := range []string{f.guest, f.paid, f.discounted, f.foreignOrder, f.unsealed} {
		orderGet(t, h, base+"/"+id, 404)
	}
	detail := orderGet(t, h, base+"/"+f.customer, 200)
	for _, private := range []string{"customer_id", "guest_contact", "shipping_snapshot", "version", "adjustments", "request_hash"} {
		if strings.Contains(detail.Body.String(), `"`+private+`"`) {
			t.Fatalf("staff/PII field leaked: %s", private)
		}
	}
	orderGet(t, h, base+"?limit=0", 400)
	orderGet(t, h, base+"?cursor=bad", 400)
	var claimed int64
	// The real mailbox-claim function refuses unverified profile equality.
	// Each maintenance call is its own TX: set the tenant in the same statement.
	f.scan("SELECT app.customer_claim_guest_orders($2) FROM (SELECT set_config('app.tenant_id',$1,true)) s", []any{f.tenant, owner}, &claimed)
	if claimed != 0 {
		t.Fatal("unverified email claimed a guest order")
	}
	f.scan("UPDATE app.customers SET verified_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2 RETURNING id::text", []any{f.tenant, owner}, &changed)
	f.scan("SELECT app.customer_claim_guest_orders($2) FROM (SELECT set_config('app.tenant_id',$1,true)) s", []any{f.tenant, owner}, &claimed)
	if claimed < 1 {
		t.Fatal("verified mailbox did not link guest history")
	}
	orderGet(t, h, base+"/"+f.guest, 200)
	orderGet(t, other, base+"/"+f.guest, 404)
	first := decodePage(t, orderGet(t, h, base+"?limit=1", 200))
	if first.NextCursor == nil {
		t.Fatal("missing history cursor")
	}
	next := decodePage(t, orderGet(t, h, base+"?limit=100&cursor="+*first.NextCursor, 200))
	if len(next.Items) == 0 {
		t.Fatal("empty history continuation")
	}
}
