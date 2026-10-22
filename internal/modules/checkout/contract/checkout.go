// Package contract owns checkout's transport-neutral actor, commands and ports.
// Dependencies: Orders/Ledger public contracts, neutral Payment Port, pure calculators.
// Callers: trusted storefront resolver, checkout service and test composition.
// Invariants: actor capability is not request data; cart prices are recomputed,
// existing orders retain sealed prices, and payment calls never hold a transaction.
package contract

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	ledger "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	orders "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/shipping"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

var (
	ErrInvalid     = errors.New("checkout: invalid input")
	ErrForbidden   = errors.New("checkout: unauthorized actor")
	ErrNotFound    = errors.New("checkout: not found")
	ErrConflict    = errors.New("checkout: state conflict")
	ErrKeyReused   = errors.New("checkout: idempotency content conflict")
	ErrUnavailable = errors.New("checkout: unavailable")
	ErrRetry       = errors.New("checkout: retry required")
)

// AuthorizedActor is produced only by a trusted resolver AFTER capability/session
// verification. guest IDs are SHA256 cookie capability digests, never plaintext.
// Construction validates shape; it does not authenticate an HTTP request.
type AuthorizedActor struct{ tenantID, kind, id string }

func NewAuthorizedActor(tenantID, kind, id string) (AuthorizedActor, error) {
	a := AuthorizedActor{tenantID, kind, id}
	if !a.Valid() {
		return AuthorizedActor{}, ErrForbidden
	}
	return a, nil
}
func (a AuthorizedActor) TenantID() string { return a.tenantID }
func (a AuthorizedActor) Kind() string     { return a.kind }
func (a AuthorizedActor) ID() string       { return a.id }
func (a AuthorizedActor) Valid() bool {
	if !tenanttx.ValidTenantID(a.tenantID) {
		return false
	}
	switch a.kind {
	case "customer":
		return tenanttx.ValidTenantID(a.id)
	case "guest":
		b, e := hex.DecodeString(a.id)
		return e == nil && len(b) == 32 && hex.EncodeToString(b) == a.id
	}
	return false
}

type ShippingAddress struct {
	Country     string `json:"country"`
	Subdivision string `json:"subdivision,omitempty"`
	Address1    string `json:"address1"`
	Address2    string `json:"address2,omitempty"`
	City        string `json:"city"`
	PostalCode  string `json:"postal_code"`
}
type StartInput struct {
	CartID          string          `json:"cart_id"`
	QuoteID         string          `json:"quote_id,omitempty"`
	BuyerContact    orders.Contact  `json:"buyer_contact"`
	ShippingAddress ShippingAddress `json:"shipping_address"`
	IdempotencyKey  string          `json:"-"`
}
type CancelInput struct {
	CheckoutID     string `json:"checkout_id"`
	IdempotencyKey string `json:"-"`
}
type Intent struct {
	ProviderRef    string                `json:"provider_ref"`
	Status         payment.PaymentStatus `json:"status"`
	NextActionKind string                `json:"next_action_kind,omitempty"`
	NextActionURL  string                `json:"next_action_url,omitempty"`
}
type Result struct {
	CheckoutID   string `json:"checkout_id"`
	OrderID      string `json:"order_id"`
	State        string `json:"state"`
	AmountMinor  int64  `json:"amount_minor"`
	Currency     string `json:"currency"`
	OrderVersion int64  `json:"order_version"`
	Payment      Intent `json:"payment"`
}
type PreviewInput struct {
	CartID, Country, Region, PostalCode string
}
type ShippingOption struct {
	RateID, Method string
	AmountMinor    int64
}
type Preview struct {
	SubtotalMinor, TaxMinor, ShippingMinor, TotalMinor int64
	Currency                                           string
	ShippingOptions                                    []ShippingOption
}
type Service interface {
	Start(context.Context, AuthorizedActor, StartInput) (Result, error)
	Confirm(context.Context, payment.VerifiedEvent) (Result, error)
	Cancel(context.Context, AuthorizedActor, CancelInput) (Result, error)
}

type BrowserReader interface {
	Preview(context.Context, AuthorizedActor, PreviewInput) (Preview, error)
	Get(context.Context, AuthorizedActor, string, string) (Result, error)
}

// ExistingOrderService adopts an already sealed customer order without carts or
// recalculation. Service stays compatible with transports that only use carts.
type ExistingOrderService interface {
	Service
	AdoptOrder(context.Context, AuthorizedActor, string, string) (Result, error)
}

// Cart is a consumption projection compatible with PR31's shopping-intent
// snapshot. No projected unit price is admitted. Adapter verifies tenant/actor
// ownership and freezes lines/version for the caller's transaction.
type CartLine struct {
	ID, SKUID string
	Quantity  int64
}
type Cart struct {
	ID       string
	Currency string
	Version  int64
	Lines    []CartLine
}
type CartReader interface {
	Read(context.Context, pgx.Tx, AuthorizedActor, string) (Cart, error)
}

// Metadata comes from trusted server configuration until catalog has canonical
// category/weight/profile columns. It cannot originate from checkout request JSON.
type SKU struct {
	ID, Code, TaxCategory string
	WeightG               int64
}
type Rules struct {
	TenantID          string
	Currency          string
	At                time.Time
	PricesIncludeTax  bool
	PriceLists        []pricing.PriceList
	Tax               tax.Ruleset
	ShippingZones     []shipping.Zone
	ShippingRates     []shipping.Rate
	ShippingProfileID string
	SKUs              map[string]SKU
	CustomerGroupID   string
}
type RulesReader interface {
	Load(context.Context, pgx.Tx, AuthorizedActor, Cart, ShippingAddress) (Rules, error)
}

// Run supplies a transaction already stamped with the requested tenant and
// effective app_runtime role. Login composition must set its local role before
// the callback; this module never elevates or opens a connection itself.
type Run func(context.Context, string, func(context.Context, pgx.Tx) error) error
type Config struct {
	Read         Run
	SnapshotCart CartReader
	Run          Run
	Cart         CartReader
	Rules        RulesReader
	Orders       orders.Service
	Ledger       ledger.Poster
	Payment      payment.Port
}
