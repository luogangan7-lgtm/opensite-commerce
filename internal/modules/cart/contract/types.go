// Package contract exposes cart intent and consumer-defined persistence ports.
// It depends on pricing, pgx transaction capability and the standard library; cart service, transport and
// future checkout consume it. Actors are trusted, prices are never persisted,
// and a strong ETag includes both intent version and current read projections.
package contract

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
)

// Beginner is supplied by the application composition root. Business code
// receives transaction capability rather than constructing a raw database pool.
type Beginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

const MaxQuantity int64 = 1_000_000

// AuthorizedActor is supplied by a trusted customer resolver or a verified
// guest capability. Staff identity, customer IDs in JSON and tenant headers
// cannot create an actor. GuestTokenHash contains SHA-256 bytes, never a token.
type AuthorizedActor struct {
	TenantID       string
	CustomerID     string
	GuestTokenHash []byte
}

type Line struct {
	SKUID             string `json:"sku_id"`
	Quantity          int64  `json:"quantity"`
	UnitAmount        int64  `json:"unit_amount"`
	LineAmount        int64  `json:"line_amount"`
	PriceListID       string `json:"price_list_id"`
	Priority          int32  `json:"priority"`
	MinQuantity       int64  `json:"min_quantity"`
	AvailableQuantity int64  `json:"available_quantity"`
	Sellable          bool   `json:"sellable"`
	PricingError      string `json:"pricing_error,omitempty"`
}

type View struct {
	ID           string `json:"id"`
	Currency     string `json:"currency"`
	Version      int64  `json:"version"`
	Lines        []Line `json:"lines"`
	Subtotal     int64  `json:"subtotal"`
	PricingError string `json:"pricing_error,omitempty"`
}

// Failed quotes have no public money amounts. The Go values are meaningful
// only when PricingError is empty; valid zero-price quotes still encode zero.
func (l Line) MarshalJSON() ([]byte, error) {
	type quoted Line
	if l.PricingError == "" {
		return json.Marshal(quoted(l))
	}
	return json.Marshal(struct {
		SKUID             string `json:"sku_id"`
		Quantity          int64  `json:"quantity"`
		AvailableQuantity int64  `json:"available_quantity"`
		Sellable          bool   `json:"sellable"`
		PricingError      string `json:"pricing_error"`
	}{l.SKUID, l.Quantity, l.AvailableQuantity, false, l.PricingError})
}

func (v View) MarshalJSON() ([]byte, error) {
	type complete View
	if v.PricingError == "" {
		return json.Marshal(complete(v))
	}
	return json.Marshal(struct {
		ID           string `json:"id"`
		Currency     string `json:"currency"`
		Version      int64  `json:"version"`
		Lines        []Line `json:"lines"`
		PricingError string `json:"pricing_error"`
	}{v.ID, v.Currency, v.Version, v.Lines, v.PricingError})
}

type Created struct {
	Cart      View   `json:"cart"`
	CartToken string `json:"cart_token,omitempty"`
}

// ETag is sensitive to price and stock changes even when intent is unchanged.
// Fixed typed fields cannot fail JSON encoding; lines are sorted by SKU before
// projection. The version prevents ABA when intent is changed and restored.
func ETag(view View) string {
	b, _ := json.Marshal(view)
	return fmt.Sprintf("\"v%d-%x\"", view.Version, sha256.Sum256(b))
}

type StoredLine struct {
	SKUID             string
	Quantity          int64
	AvailableQuantity int64
	Active            bool
}

// Snapshot is one transaction's live projection, not a stored price snapshot.
type Snapshot struct {
	ID, TenantID, Currency string
	Version                int64
	Lines                  []StoredLine
	PriceLists             []pricing.PriceList
	At                     time.Time
}

// Repository keeps ownership checks, ETag admission and the absolute mutation
// in one transaction. Check performs only pure calculation while the cart row
// is locked, so no write can slip between admission and version advancement.
type Repository interface {
	Create(context.Context, AuthorizedActor, string) (Snapshot, error)
	Read(context.Context, AuthorizedActor, string) (Snapshot, error)
	Change(context.Context, AuthorizedActor, string, string, int64, func(Snapshot) error, func(Snapshot) error) (Snapshot, error)
}

type actorKey struct{}

func WithActor(ctx context.Context, actor AuthorizedActor) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

func Actor(ctx context.Context) (AuthorizedActor, bool) {
	a, ok := ctx.Value(actorKey{}).(AuthorizedActor)
	return a, ok
}
