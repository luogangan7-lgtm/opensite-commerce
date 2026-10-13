// Responsibilities: bind an export task to a tenant and opaque request ID.
// Dependencies: existing Signer; producer and River worker use this proof.
// Invariant: payloads carry no customer PII, file paths or download capability.
package jobs

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/riverqueue/river"
)

const CustomerDataExportKind = "customer_data_export"

type CustomerDataExportArgs struct {
	TenantID  string `json:"tenant_id"`
	RequestID string `json:"request_id"`
	Proof     string `json:"proof"`
}

func (CustomerDataExportArgs) Kind() string { return CustomerDataExportKind }

// CustomerDataExportQueue is polled only by workers with the privacy service,
// so a rolling/mixed deployment cannot cancel exports as unknown kinds.
const CustomerDataExportQueue = "privacy_exports"

func (CustomerDataExportArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: CustomerDataExportQueue}
}
func (s *Signer) customerExportProof(a CustomerDataExportArgs) []byte {
	h := hmac.New(sha256.New, s.key)
	fmt.Fprintf(h, "opensite-job-v1\x00%s\x00%s\x00%s", CustomerDataExportKind, a.TenantID, a.RequestID)
	return h.Sum(nil)
}
func (s *Signer) customerExportArgs(tenant, request string) (CustomerDataExportArgs, error) {
	a := CustomerDataExportArgs{TenantID: tenant, RequestID: request}
	if s == nil || len(s.key) < 32 || !validTenant(tenant) || !validTenant(request) {
		return a, errors.New("invalid export job context")
	}
	a.Proof = hex.EncodeToString(s.customerExportProof(a))
	return a, nil
}
func (s *Signer) ValidateCustomerExport(a CustomerDataExportArgs) error {
	if s == nil || len(s.key) < 32 || !validTenant(a.TenantID) || !validTenant(a.RequestID) {
		return errors.New("untrusted export job context")
	}
	b, err := hex.DecodeString(a.Proof)
	if err != nil || !hmac.Equal(b, s.customerExportProof(a)) {
		return errors.New("untrusted export job context")
	}
	return nil
}
