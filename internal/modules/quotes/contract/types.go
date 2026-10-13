// Package contract owns quote values and the trusted identity seam used by HTTP.
// Dependencies: tax/shipping pure calculator contracts and stdlib; callers: quotes and HTTP.
// Invariants: actors cannot be constructed from request DTOs; all amounts are minor units.
package contract

import (
	"context"
	"errors"
	"fmt"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/shipping"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
	"time"
)

var (
	ErrUnauthorized         = errors.New("quote identity required")
	ErrForbidden            = errors.New("quote authority denied")
	ErrNotFound             = errors.New("quote not found")
	ErrInvalid              = errors.New("invalid quote input")
	ErrPreconditionRequired = errors.New("quote If-Match required")
	ErrPrecondition         = errors.New("quote version does not match")
	ErrConflict             = errors.New("quote state conflict")
	ErrIdempotencyReused    = errors.New("quote idempotency key reused")
	ErrUnavailable          = errors.New("quote storage unavailable")
	ErrOverflow             = errors.New("quote amount overflow")
)

type ActorIdentity struct{ TenantID, StaffID, CustomerID, CompanyID string }
type AuthorizedActor struct{ identity ActorIdentity }
type actorKey struct{}

// WithTrustedActor is the authentication adapter/test seam, never a JSON input.
// Production adapters must verify session/key and tenant scope before calling it.
func WithTrustedActor(ctx context.Context, identity ActorIdentity) context.Context {
	return context.WithValue(ctx, actorKey{}, identity)
}
func ActorFromContext(ctx context.Context) (AuthorizedActor, error) {
	i, ok := ctx.Value(actorKey{}).(ActorIdentity)
	if !ok || !ValidID(i.TenantID) || (i.StaffID == "") == (i.CustomerID == "") {
		return AuthorizedActor{}, ErrUnauthorized
	}
	if i.StaffID != "" {
		if !ValidID(i.StaffID) || i.CompanyID != "" {
			return AuthorizedActor{}, ErrUnauthorized
		}
	} else if !ValidID(i.CustomerID) || !ValidID(i.CompanyID) {
		return AuthorizedActor{}, ErrUnauthorized
	}
	return AuthorizedActor{identity: i}, nil
}
func (a AuthorizedActor) TenantID() string { return a.identity.TenantID }
func (a AuthorizedActor) ID() string {
	if a.identity.StaffID != "" {
		return a.identity.StaffID
	}
	return a.identity.CustomerID
}
func (a AuthorizedActor) Kind() string {
	if a.identity.StaffID != "" {
		return "staff"
	}
	if a.identity.CustomerID != "" {
		return "customer"
	}
	return ""
}
func (a AuthorizedActor) CompanyID() string { return a.identity.CompanyID }
func (a AuthorizedActor) Valid() bool {
	return ValidID(a.TenantID()) && ValidID(a.ID()) && (a.Kind() == "staff" || ValidID(a.CompanyID()))
}
func ValidID(id string) bool {
	if len(id) != 36 || id == "00000000-0000-0000-0000-000000000000" {
		return false
	}
	for i, c := range []byte(id) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

type Action string

const (
	Send     Action = "send"
	Accept   Action = "accept"
	Approve  Action = "approve"
	Reject   Action = "reject"
	Withdraw Action = "withdraw"
	Expire   Action = "expire"
)

// MaxStoredJSONBytes bounds a stored snapshot and its replay receipt. It is
// derived from MaxQuoteLines: a line carries a <=1000-rune description (<=6
// escaped JSON bytes per rune) plus line/tax fields, measured at ~6.9 KiB worst
// case; 8 KiB per line plus 64 KiB of quote-level fields stays under the 1 MiB
// hard cap enforced by the 026 quotes CHECK constraints.
const (
	MaxQuoteLines      = 100
	maxLineJSONBytes   = 8 << 10
	MaxStoredJSONBytes = 64<<10 + MaxQuoteLines*maxLineJSONBytes
)

type WriteOptions struct{ IfMatch, IdempotencyKey string }
type LineInput struct {
	ProductID   string `json:"product_id,omitempty"`
	VariantID   string `json:"variant_id,omitempty"`
	Description string `json:"description"`
	Quantity    int64  `json:"quantity"`
	UnitPrice   int64  `json:"unit_price"`
	TaxCategory string `json:"tax_category"`
}
type CreateInput struct {
	SiteID            string          `json:"site_id"`
	InquiryID         string          `json:"inquiry_id"`
	CompanyID         string          `json:"company_id"`
	Currency          string          `json:"currency"`
	ExpiresAt         time.Time       `json:"expires_at"`
	Lines             []LineInput     `json:"lines"`
	Destination       tax.Destination `json:"destination"`
	ShippingProfileID string          `json:"shipping_profile_id"`
	WeightG           int64           `json:"weight_g"`
}
type Line struct {
	LineNo      int32          `json:"line_no"`
	ProductID   string         `json:"product_id,omitempty"`
	VariantID   string         `json:"variant_id,omitempty"`
	Description string         `json:"description"`
	Quantity    int64          `json:"quantity"`
	UnitPrice   int64          `json:"unit_price"`
	LineTotal   int64          `json:"line_total"`
	Snapshot    tax.LineResult `json:"snapshot"`
}
type Snapshot struct {
	Calculator        string          `json:"calculator"`
	Currency          string          `json:"currency"`
	Lines             []Line          `json:"lines"`
	Tax               tax.Result      `json:"tax"`
	Shipping          shipping.Result `json:"shipping"`
	Subtotal          int64           `json:"subtotal"`
	TaxAmount         int64           `json:"tax_amount"`
	ShippingAmount    int64           `json:"shipping_amount"`
	Total             int64           `json:"total"`
	CalculatedAt      time.Time       `json:"calculated_at"`
	Destination       tax.Destination `json:"destination"`
	ShippingProfileID string          `json:"shipping_profile_id"`
	WeightG           int64           `json:"weight_g"`
}
type Quote struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	SiteID     string    `json:"site_id"`
	InquiryID  string    `json:"inquiry_id"`
	CompanyID  string    `json:"company_id"`
	Status     string    `json:"status"`
	Version    int64     `json:"version"`
	Currency   string    `json:"currency"`
	ExpiresAt  time.Time `json:"expires_at"`
	CreatedBy  string    `json:"created_by"`
	AcceptedBy string    `json:"accepted_by,omitempty"`
	Snapshot   Snapshot  `json:"snapshot"`
}

func (q Quote) ETag() string { return fmt.Sprintf("\"quote-%s-v%d\"", q.ID, q.Version) }

type CalculationRules struct {
	Tax           tax.Ruleset
	IncludeTax    bool
	ShippingZones []shipping.Zone
	ShippingRates []shipping.Rate
}
type InquiryInput struct {
	ProductID string   `json:"product_id,omitempty"`
	Quantity  *float64 `json:"quantity,omitempty"`
	SiteID    string   `json:"site_id"`
	Email     string   `json:"email"`
	Name      string   `json:"name"`
	Message   string   `json:"message"`
}
type Inquiry struct {
	ID     string `json:"id"`
	SiteID string `json:"site_id"`
	Status string `json:"status"`
}
type inquiryKey struct{}
type InquiryIdentity struct{ TenantID, SiteID string }

// WithTrustedInquiry stamps a host/site mapping resolved by the application, never request JSON.
func WithTrustedInquiry(ctx context.Context, tenantID, siteID string) context.Context {
	return context.WithValue(ctx, inquiryKey{}, InquiryIdentity{TenantID: tenantID, SiteID: siteID})
}
func InquiryFromContext(ctx context.Context) (InquiryIdentity, error) {
	i, ok := ctx.Value(inquiryKey{}).(InquiryIdentity)
	if !ok || !ValidID(i.TenantID) || !ValidID(i.SiteID) {
		return InquiryIdentity{}, ErrUnauthorized
	}
	return i, nil
}
