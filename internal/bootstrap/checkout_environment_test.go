// Responsibility: pin explicit dev/test provider policy and actor provenance.
// Dependencies: bootstrap private parser and public actor contracts.
// Invariant: no implicit Fake, production Fake, or URL-only quote capability.
package bootstrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	cc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	pc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	checkouthttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQuoteCheckoutEnvironmentRequiresExplicitDevelopmentCapability(t *testing.T) {
	key := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	for _, c := range []struct {
		name, environment, enabled, key string
		want, fail                      bool
	}{
		{"default-deny", "", "", "", false, false},
		{"production-disabled", "production", "false", "", false, false},
		{"implicit-environment", "", "true", key, false, true},
		{"production-fake", "production", "true", key, false, true},
		{"partial-secret", "test", "false", key, false, true},
		{"partial-enabled", "test", "true", "", false, true},
		{"noncanonical-flag", "test", "1", key, false, true},
		{"bad-key", "test", "true", "invalid", false, true},
		{"development", "development", "true", key, true, false},
		{"test", "test", "true", key, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			values := map[string]string{"OPENSITE_ENVIRONMENT": c.environment, "OPENSITE_CHECKOUT_FAKE_ENABLED": c.enabled, "OPENSITE_CHECKOUT_FAKE_SIGNING_KEY": c.key}
			out, err := checkoutEnvironment(func(k string) string { return values[k] })
			if (err != nil) != c.fail || out.enabled != c.want {
				t.Fatalf("enabled=%v error=%v", out.enabled, err)
			}
		})
	}
	if _, err := composeFakeCheckout(Config{}, runtimeQuoteRunner{}, fakeCheckoutEnvironment{enabled: true, key: make([]byte, 32), environment: "test"}); err == nil {
		t.Fatal("missing persistence/customer capability accepted")
	}
	t.Setenv("OPENSITE_ENVIRONMENT", "production")
	t.Setenv("OPENSITE_CHECKOUT_FAKE_ENABLED", "true")
	t.Setenv("OPENSITE_CHECKOUT_FAKE_SIGNING_KEY", key)
	if _, close, err := FromEnvironment(context.Background(), Config{}); err == nil {
		if close != nil {
			close()
		}
		t.Fatal("production environment composed Fake")
	}
}

func TestQuoteRuntimeAdmissionRequiresVerifiedActor(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const buyer = "22222222-2222-4222-8222-222222222222"
	const company = "33333333-3333-4333-8333-333333333333"
	writes, snapshots := 0, 0
	r := runtimeQuoteRunner{run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
		writes++
		return fn(ctx, nil)
	}, snapshot: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
		snapshots++
		return fn(ctx, nil)
	}}
	fn := func(context.Context, pgx.Tx) error { return nil }
	if err := r.InTenant(context.Background(), tenant, fn); err == nil {
		t.Fatal("URL tenant alone accepted")
	}
	verifiedCustomer := cc.WithActor(context.Background(), cc.Actor{TenantID: tenant, CustomerID: buyer})
	if err := r.InTenant(verifiedCustomer, tenant, fn); err != nil || writes != 0 || snapshots != 1 {
		t.Fatalf("customer must use snapshot: %v", err)
	}
	admittedBuyer := qc.WithTrustedActor(verifiedCustomer, qc.ActorIdentity{TenantID: tenant, CustomerID: buyer, CompanyID: company})
	if err := r.InTenant(admittedBuyer, tenant, fn); err != nil || writes != 1 {
		t.Fatalf("admitted buyer: %v", err)
	}
	if err := r.InTenant(admittedBuyer, buyer, fn); err == nil {
		t.Fatal("actor tenant mismatch accepted")
	}
}

// The mapped receiver is composed first; adding quote commands must preserve it.
type quoteCompositionCustomer struct{ cc.Service }
type quoteCompositionReceiver struct{ pc.Receiver }

func TestQuoteFakeCompositionPreservesMappedReceiver(t *testing.T) {
	receiver := &quoteCompositionReceiver{}
	run := func(context.Context, string, func(context.Context, pgx.Tx) error) error { return nil }
	out, err := composeFakeCheckout(Config{
		Customer: &quoteCompositionCustomer{}, CustomerSecret: bytes.Repeat([]byte{1}, 32),
		Checkout: &checkouthttp.Config{Events: receiver, FakeEnabled: true, Environment: "test"},
	}, runtimeQuoteRunner{run: run, snapshot: run}, fakeCheckoutEnvironment{
		enabled: true, key: bytes.Repeat([]byte{2}, 32), environment: "test",
	})
	if err != nil {
		t.Fatalf("mapped receiver rejected during quote composition: %v", err)
	}
	if out.Checkout == nil || out.Checkout.Service == nil || out.Checkout.Events != receiver || !out.Checkout.FakeEnabled || out.Checkout.Environment != "test" || out.QuoteConversion == nil || out.Quotes == nil {
		t.Fatal("quote commands and mapped payment receiver were not composed together")
	}
}

func TestQuoteFakeConflictingSigningAuthoritiesFailBeforeIO(t *testing.T) {
	t.Setenv("OPENSITE_ENVIRONMENT", "test")
	t.Setenv("OPENSITE_CHECKOUT_FAKE_ENABLED", "true")
	t.Setenv("OPENSITE_CHECKOUT_FAKE_SIGNING_KEY", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	t.Setenv("OPENSITE_FAKE_PSP", "1")
	t.Setenv("OPENSITE_FAKE_WEBHOOK_KEY", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)))
	_, closeRuntime, err := FromEnvironment(context.Background(), Config{})
	if closeRuntime != nil {
		closeRuntime()
		t.Fatal("conflicting signing authorities initialized database capabilities")
	}
	if err == nil || err.Error() != "Fake checkout and payment signing keys conflict" {
		t.Fatalf("conflicting signing authorities were not refused before IO: %v", err)
	}
}

// Buyer capability admission must never delegate to staff cookie or scope hooks.
type quoteStaffAuthority struct {
	httpapi.Authenticator
	rechecks, responses int
}

func (a *quoteStaffAuthority) ResolveTransactionAuthorization(context.Context, *http.Request, httpapi.RouteSpec) (func(context.Context, *db.Queries) error, error) {
	a.rechecks++
	return nil, nil
}

func (a *quoteStaffAuthority) AuthenticationResponse(context.Context, http.ResponseWriter, *http.Request, httpapi.RouteSpec) {
	a.responses++
}

func TestQuoteBuyerDoesNotAcquireStaffAuthority(t *testing.T) {
	for _, access := range []httpapi.Access{httpapi.Storefront, httpapi.BuyerCompany, httpapi.AdminOrAPIKey} {
		t.Run(string(access), func(t *testing.T) {
			staff := &quoteStaffAuthority{}
			auth := routeAuthenticator{staff: staff}
			spec := httpapi.RouteSpec{Access: access}
			request := httptest.NewRequest(http.MethodPost, "/", nil)
			check, err := auth.ResolveTransactionAuthorization(request.Context(), request, spec)
			auth.AuthenticationResponse(request.Context(), httptest.NewRecorder(), request, spec)
			want := 0
			if access == httpapi.AdminOrAPIKey {
				want = 1
			}
			if err != nil || check != nil || staff.rechecks != want || staff.responses != want {
				t.Fatalf("QUOTE_BUYER_STAFF_AUTHORITY_LEAK rechecks=%d responses=%d want=%d error=%v", staff.rechecks, staff.responses, want, err)
			}
		})
	}
}

func TestB2BInquiryRuntimeStamp(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const site = "22222222-2222-4222-8222-222222222222"
	writes := 0
	r := runtimeQuoteRunner{run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
		writes++
		return fn(ctx, nil)
	}}
	ctx := qc.WithTrustedInquiry(context.Background(), tenant, site)
	fn := func(context.Context, pgx.Tx) error { return nil }
	if err := r.InTenant(ctx, tenant, fn); err != nil || writes != 1 {
		t.Fatalf("verified inquiry cannot persist: %v", err)
	}
	if err := r.InTenant(ctx, site, fn); err == nil || writes != 1 {
		t.Fatal("inquiry stamp authorized another tenant")
	}
}
