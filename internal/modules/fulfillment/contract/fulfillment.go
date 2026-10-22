// Package contract owns fulfillment's public command values and errors.
// Dependencies: pgx and UUID validation; callers: authorized admin API/services.
// Invariants: shipments allocate already committed order stock, never decrement
// it a second time; tenant/version/quantity/idempotency remain DB-enforced.
package contract

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrInvalid      = errors.New("invalid fulfillment input")
	ErrNotFound     = errors.New("fulfillment resource not found")
	ErrConflict     = errors.New("fulfillment state or quantity conflict")
	ErrPrecondition = errors.New("fulfillment order version changed")
	ErrKeyReused    = errors.New("fulfillment idempotency key reused")
	ErrRetry        = errors.New("fulfillment transaction retry required")
	ErrUnavailable  = errors.New("fulfillment storage unavailable")
)

type Line struct {
	OrderLineID string `json:"order_line_id"`
	Quantity    int64  `json:"quantity"`
}
type CreateInput struct {
	OrderID                                 string
	ExpectedVersion                         int64
	IdempotencyKey, Carrier, TrackingNumber string
	Lines                                   []Line
}
type Shipment struct {
	ID           string         `json:"id"`
	OrderID      string         `json:"order_id"`
	OrderVersion int64          `json:"order_version"`
	Status       string         `json:"status"`
	Lines        []ShipmentLine `json:"lines"`
}
type ShipmentLine struct {
	ID          string `json:"id"`
	OrderLineID string `json:"order_line_id"`
	Quantity    int64  `json:"quantity"`
}
type Service interface {
	CreateShipment(context.Context, pgx.Tx, string, CreateInput) (Shipment, error)
}

func ValidID(s string) bool {
	id, e := uuid.Parse(s)
	return e == nil && id != uuid.Nil && id.String() == s
}
func ValidKey(s string) bool {
	return len(s) > 0 && len(s) <= 128 && !strings.ContainsFunc(s, func(c rune) bool { return c < '!' || c > '~' })
}
func (in CreateInput) Validate() error {
	if !ValidID(in.OrderID) || in.ExpectedVersion <= 0 || !ValidKey(in.IdempotencyKey) || len(in.Lines) == 0 || len(in.Lines) > 1000 {
		return ErrInvalid
	}
	if strings.TrimSpace(in.Carrier) == "" {
		return ErrInvalid
	}
	for i, s := range []string{in.Carrier, in.TrackingNumber} {
		limit := 128
		if i == 1 {
			limit = 256
		}
		if utf8.RuneCountInString(s) > limit || !utf8.ValidString(s) || strings.ContainsAny(s, "\x00\r\n") {
			return ErrInvalid
		}
	}
	seen := make(map[string]bool, len(in.Lines))
	for _, l := range in.Lines {
		if !ValidID(l.OrderLineID) || l.Quantity <= 0 || seen[l.OrderLineID] {
			return ErrInvalid
		}
		seen[l.OrderLineID] = true
	}
	return nil
}
