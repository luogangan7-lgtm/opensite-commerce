// Responsibilities: mint and verify short-lived revision-pinned preview grants.
// Dependencies: stdlib cryptography and publish contract; callers: write facade.
// Invariants: exact audience/scope, random nonce, bounded lifetime; verification precedes queries.
package service

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
	"strings"
	"time"
)

const previewAudience = "opensite.cms.preview.v1"

type PreviewSigner struct {
	key []byte
	now func() time.Time
}

func NewPreviewSigner(key []byte, now func() time.Time) (*PreviewSigner, error) {
	if len(key) < 32 || len(key) > 4096 {
		return nil, contract.ErrPreviewDenied
	}
	if now == nil {
		now = time.Now
	}
	return &PreviewSigner{key: append([]byte(nil), key...), now: now}, nil
}
func (s *PreviewSigner) mac(data string) []byte {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
func (s *PreviewSigner) Issue(tenant, site, draft string, revision int64, ttl time.Duration) (string, time.Time, error) {
	if s == nil || !tenanttx.ValidTenantID(tenant) || !tenanttx.ValidTenantID(site) || !tenanttx.ValidTenantID(draft) || revision < 1 || ttl < time.Second || ttl > contract.MaxPreviewTTL {
		return "", time.Time{}, contract.ErrPreviewDenied
	}
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return "", time.Time{}, contract.ErrUnavailable
	}
	now := s.now().UTC().Truncate(time.Second)
	expires := now.Add(ttl).Truncate(time.Second)
	c := contract.PreviewClaims{Audience: previewAudience, TenantID: tenant, SiteID: site, DraftID: draft, Revision: revision, IssuedAt: now.Unix(), ExpiresAt: expires.Unix(), Nonce: base64.RawURLEncoding.EncodeToString(nonce)}
	data, err := json.Marshal(c)
	if err != nil {
		return "", time.Time{}, contract.ErrUnavailable
	}
	body := base64.RawURLEncoding.EncodeToString(data)
	return body + "." + base64.RawURLEncoding.EncodeToString(s.mac(body)), expires, nil
}
func (s *PreviewSigner) Verify(token, tenant, site, draft string) (contract.PreviewClaims, error) {
	fail := func() (contract.PreviewClaims, error) { return contract.PreviewClaims{}, contract.ErrPreviewDenied }
	if s == nil || len(token) > 2048 {
		return fail()
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return fail()
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(sig) != sha256.Size || !hmac.Equal(sig, s.mac(parts[0])) {
		return fail()
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return fail()
	}
	var c contract.PreviewClaims
	if StrictDecode(raw, &c) != nil {
		return fail()
	}
	nonce, err := base64.RawURLEncoding.Strict().DecodeString(c.Nonce)
	now := s.now().Unix()
	if err != nil || len(nonce) != 24 || c.Audience != previewAudience || !tenanttx.ValidTenantID(c.TenantID) || !tenanttx.ValidTenantID(c.SiteID) || !tenanttx.ValidTenantID(c.DraftID) || c.Revision < 1 || c.IssuedAt > now || c.ExpiresAt <= now || c.ExpiresAt <= c.IssuedAt || c.ExpiresAt-c.IssuedAt > int64(contract.MaxPreviewTTL/time.Second) {
		return fail()
	}
	if c.TenantID != tenant || c.SiteID != site || c.DraftID != draft {
		return contract.PreviewClaims{}, contract.ErrNotFound
	}
	return c, nil
}
