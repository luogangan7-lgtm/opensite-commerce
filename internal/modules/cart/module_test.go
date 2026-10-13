// Module authority tests exercise the trusted resolver/capability seam directly.
// They depend on stdlib and public cart/platform contracts, never a raw pool.
// Bootstrap calls the same authenticator; ambiguous credentials fail closed.
package cart_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func TestActorCapabilityAdmission(t *testing.T) {
	const tenant = "10000000-0000-4000-8000-000000000001"
	const customer = "20000000-0000-4000-8000-000000000001"
	token := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	for _, tc := range []struct {
		name, operation string
		headers         []string
		actor           contract.AuthorizedActor
		allowed         bool
	}{
		{"anonymous creation", "createCart", nil, contract.AuthorizedActor{}, true},
		{"own guest", "getCart", []string{token}, contract.AuthorizedActor{}, true},
		{"missing guest", "getCart", nil, contract.AuthorizedActor{}, false},
		{"malformed", "getCart", []string{"invalid"}, contract.AuthorizedActor{}, false},
		{"duplicate token", "getCart", []string{token, token}, contract.AuthorizedActor{}, false},
		{"padded token", "getCart", []string{token + "="}, contract.AuthorizedActor{}, false},
		{"customer", "getCart", nil, contract.AuthorizedActor{TenantID: tenant, CustomerID: customer}, true},
		{"customer plus guest", "getCart", []string{token}, contract.AuthorizedActor{TenantID: tenant, CustomerID: customer}, false},
		{"foreign customer tenant", "getCart", nil, contract.AuthorizedActor{TenantID: customer, CustomerID: customer}, false},
		{"partial actor", "getCart", []string{token}, contract.AuthorizedActor{TenantID: tenant}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := cart.New(nil, func(context.Context, *http.Request) (contract.AuthorizedActor, error) { return tc.actor, nil })
			r := httptest.NewRequest("GET", "/", nil)
			r.SetPathValue("tenant_id", tenant)
			for _, value := range tc.headers {
				r.Header.Add("X-Cart-Token", value)
			}
			ctx, err := m.Authenticate(r.Context(), r, platform.RouteSpec{Access: platform.Storefront, OperationID: tc.operation})
			if !tc.allowed {
				var e *apperrors.Error
				if !errors.As(err, &e) || e.Status != 404 {
					t.Fatalf("denial=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			actor, ok := contract.Actor(ctx)
			if !ok || actor.TenantID != tenant {
				t.Fatalf("actor=%+v", actor)
			}
			if len(tc.headers) == 1 {
				hash := sha256.Sum256([]byte(token))
				if string(actor.GuestTokenHash) != string(hash[:]) {
					t.Fatal("encoded capability hash differs")
				}
			}
		})
	}
}

func TestAnonymousCreationDefaultIPLimit(t *testing.T) {
	m := cart.New(nil, nil)
	for i := 0; i < 31; i++ {
		r := httptest.NewRequest("POST", "/", nil)
		r.SetPathValue("tenant_id", "10000000-0000-4000-8000-000000000001")
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("X-Forwarded-For", "198.51.100."+fmt.Sprint(i))
		_, err := m.Authenticate(r.Context(), r, platform.RouteSpec{Access: platform.Storefront, OperationID: "createCart"})
		if i < 30 && err != nil {
			t.Fatal(err)
		}
		if i == 30 {
			var failure *apperrors.Error
			if !errors.As(err, &failure) || failure.Status != 429 {
				t.Fatalf("31st anonymous create = %v; want 429", err)
			}
		}
	}
}
