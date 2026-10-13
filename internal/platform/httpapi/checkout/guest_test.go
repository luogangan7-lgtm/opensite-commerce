// Responsibility: pin guest proof admission to one tenant, origin and cart.
// Dependencies: stdlib HTTP/HMAC and checkout resolver; no database fixture.
// Invariant: a valid MAC alone never grants access without cart ownership.
package checkout_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	contract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	checkout "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
)

func TestGuestResolverBindsTenantCartAndProof(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	token := base64.RawURLEncoding.EncodeToString([]byte("abcdef0123456789abcdef012345678901"))
	calls := 0
	resolver, err := checkout.NewGuestResolver(checkout.GuestConfig{TenantID: tenant, Origin: origin, Key: key, VerifyCart: func(_ context.Context, actor contract.AuthorizedActor, cartID string) error {
		calls++
		if actor.Kind() != "guest" || actor.TenantID() != tenant || cartID != cart {
			return errors.New("owner mismatch")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	request := func(target, cartID, suppliedOrigin, proof string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/t/"+target+"/checkout/"+checkoutID, nil)
		r.SetPathValue("tenant_id", target)
		r.Header.Set("Origin", suppliedOrigin)
		r.Header.Set("X-Cart-Token", token)
		r.Header.Set("X-Cart-ID", cartID)
		r.Header.Set("X-CSRF-Token", proof)
		return r
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("checkout:" + tenant + ":" + token + ":" + origin))
	proof := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if actor, err := resolver.Resolve(context.Background(), request(tenant, cart, origin, proof)); err != nil || !actor.Valid() || calls != 1 {
		t.Fatalf("valid guest proof rejected: actor=%v err=%v calls=%d", actor, err, calls)
	}
	for _, r := range []*http.Request{request(otherTenant, cart, origin, proof), request(tenant, cart, "https://forged.example.invalid", proof), request(tenant, cart, origin, base64.RawURLEncoding.EncodeToString(make([]byte, 32))), request(tenant, orderID, origin, proof)} {
		if _, err := resolver.Resolve(context.Background(), r); err == nil {
			t.Fatal("unbound guest request admitted")
		} else {
			var denial *apperrors.Error
			if !errors.As(err, &denial) || denial.Status != http.StatusNotFound {
				t.Fatalf("capability denial must be 404: %v", err)
			}
		}
	}
	if calls != 2 {
		t.Fatalf("cart verifier was bypassed or called before proof admission: %d", calls)
	}
}
