// Package jobs owns signed internal-task admission. The producer derives tenant
// context from an authorized transaction; the worker validates its proof before
// invoking a controlled service. River's at-least-once delivery grants no rights.
package jobs

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const InventoryExpireKind = "inventory_expire_due"

type InventoryExpireArgs struct {
	TenantID string `json:"tenant_id"`
	Batch    int    `json:"batch"`
	Proof    string `json:"proof"`
}

func (InventoryExpireArgs) Kind() string { return InventoryExpireKind }

type Signer struct{ key []byte }

func NewSigner(key []byte) (*Signer, error) {
	if len(key) < 32 {
		return nil, errors.New("job signing key requires at least 32 bytes")
	}
	return &Signer{key: append([]byte(nil), key...)}, nil
}
func validTenant(s string) bool {
	if len(s) != 36 || s == "00000000-0000-0000-0000-000000000000" {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (s *Signer) proof(a InventoryExpireArgs) []byte {
	h := hmac.New(sha256.New, s.key)
	fmt.Fprintf(h, "opensite-job-v1\x00%s\x00%s\x00%d", InventoryExpireKind, a.TenantID, a.Batch)
	return h.Sum(nil)
}
func (s *Signer) inventoryArgs(tenant string, batch int) (InventoryExpireArgs, error) {
	a := InventoryExpireArgs{TenantID: strings.ToLower(tenant), Batch: batch}
	if s == nil || len(s.key) < 32 || !validTenant(a.TenantID) || batch < 1 || batch > 1000 {
		return a, errors.New("invalid job context")
	}
	a.Proof = hex.EncodeToString(s.proof(a))
	return a, nil
}
func (s *Signer) Validate(a InventoryExpireArgs) error {
	if s == nil || len(s.key) < 32 || !validTenant(a.TenantID) || a.Batch < 1 || a.Batch > 1000 {
		return errors.New("untrusted job context")
	}
	b, err := hex.DecodeString(a.Proof)
	if err != nil || !hmac.Equal(b, s.proof(a)) {
		return errors.New("untrusted job context")
	}
	return nil
}
