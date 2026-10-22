// Package checkout verifies a server-to-server guest cart proof before issuing
// the checkout actor. The proof binds one configured storefront origin, tenant,
// cart token, and the cart owner read from the runtime database.
package checkout

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"

	"github.com/google/uuid"
	contract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
)

type GuestCartVerifier func(context.Context, contract.AuthorizedActor, string) error
type GuestConfig struct {
	TenantID   string
	Origin     string
	Key        []byte
	VerifyCart GuestCartVerifier
}
type GuestResolver struct{ config GuestConfig }

func NewGuestResolver(c GuestConfig) (*GuestResolver, error) {
	id, err := uuid.Parse(c.TenantID)
	u, urlErr := url.Parse(c.Origin)
	if err != nil || id == uuid.Nil || id.String() != c.TenantID || urlErr != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || len(c.Key) < 32 || c.VerifyCart == nil {
		return nil, errors.New("invalid storefront guest resolver configuration")
	}
	c.Key = bytes.Clone(c.Key)
	return &GuestResolver{config: c}, nil
}
func singleHeader(r *http.Request, name string) string {
	values := r.Header.Values(name)
	if len(values) != 1 {
		return ""
	}
	return values[0]
}
func (g *GuestResolver) Resolve(ctx context.Context, r *http.Request) (contract.AuthorizedActor, error) {
	var zero contract.AuthorizedActor
	if r.PathValue("tenant_id") != g.config.TenantID {
		return zero, notFound()
	}
	origin := singleHeader(r, "Origin")
	token := singleHeader(r, "X-Cart-Token")
	proof := singleHeader(r, "X-CSRF-Token")
	cartID := singleHeader(r, "X-Cart-ID")
	parsed, err := uuid.Parse(cartID)
	rawToken, tokenErr := base64.RawURLEncoding.DecodeString(token)
	rawProof, proofErr := base64.RawURLEncoding.DecodeString(proof)
	if origin != g.config.Origin || err != nil || parsed == uuid.Nil || parsed.String() != cartID || tokenErr != nil || base64.RawURLEncoding.EncodeToString(rawToken) != token || proofErr != nil || len(rawProof) != sha256.Size || base64.RawURLEncoding.EncodeToString(rawProof) != proof {
		return zero, notFound()
	}
	mac := hmac.New(sha256.New, g.config.Key)
	_, _ = mac.Write([]byte("checkout:" + g.config.TenantID + ":" + token + ":" + origin))
	if !hmac.Equal(rawProof, mac.Sum(nil)) {
		return zero, notFound()
	}
	hash := sha256.Sum256([]byte(token))
	actor, err := contract.NewAuthorizedActor(g.config.TenantID, "guest", hex.EncodeToString(hash[:]))
	if err != nil {
		return zero, unauthenticated()
	}
	if err := g.config.VerifyCart(ctx, actor, cartID); err != nil {
		return zero, notFound()
	}
	return actor, nil
}
