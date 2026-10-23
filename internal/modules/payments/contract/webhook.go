// Package contract exposes payment webhook admission to HTTP/static composition.
// Dependencies: neutral Payment Port and caller-owned pgx transactions. No raw
// body becomes authority: Run maps an authenticated provider account in the DB.
package contract

import (
	"context"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"time"
)

type Receipt struct {
	EventID string `json:"event_id"`
	Status  string `json:"status"`
}
type Receiver interface {
	Receive(context.Context, []byte, string) (Receipt, error)
}

// Run uses the DB's immutable fake/test account mapping, locks it, sets the
// effective runtime role and tenant, and passes DB time. It commits iff fn succeeds.
type Run func(context.Context, string, func(context.Context, pgx.Tx, string, time.Time) error) error
type Apply func(context.Context, pgx.Tx, payment.VerifiedEvent) (bool, error)
type Config struct {
	Key         []byte
	Run         Run
	Apply       Apply
	Provider    string
	TerminalRun Run
	// Admit is mandatory for real-provider create/capture; implementation reads
	// durable cart consent through Checkout's public admission capability.
	Admit func(context.Context, payment.CreatePaymentIntent, bool) error
}
