// Package quotes exposes caller-owned transaction ports for inquiry and quote lifecycle.
// Dependencies: quote contract/private service, pgx; callers: authorized HTTP/bootstrap.
// Invariants: no order/payment creation; quotes freeze amounts and deny untrusted actors.
package quotes

import (
	"context"
	"github.com/jackc/pgx/v5"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/internal/service"
	"time"
)

type Module struct{ now func() time.Time }

func New() *Module { return &Module{now: time.Now} }
func NewWithClock(now func() time.Time) *Module {
	if now == nil {
		now = time.Now
	}
	return &Module{now: now}
}
func CalculateSnapshot(in c.CreateInput, rules c.CalculationRules, at time.Time) (c.Snapshot, error) {
	return service.Calculate(in, rules, at)
}
func ValidateTransition(q c.Quote, action c.Action, kind, id string, at time.Time) error {
	return service.ValidateTransition(q, action, kind, id, at)
}
func (m *Module) Create(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, in c.CreateInput, o c.WriteOptions) (c.Quote, error) {
	return service.Create(ctx, tx, a, in, o, m.now())
}
func (m *Module) Get(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, id string) (c.Quote, error) {
	return service.Get(ctx, tx, a, id)
}
func (m *Module) Transition(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, id string, action c.Action, o c.WriteOptions) (c.Quote, error) {
	return service.Transition(ctx, tx, a, id, action, o, m.now())
}
func (m *Module) GetAcceptedQuote(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, id string) (c.Quote, error) {
	quote, err := service.Get(ctx, tx, a, id)
	if err != nil {
		return c.Quote{}, err
	}
	if quote.Status != "approved" || !m.now().Before(quote.ExpiresAt) {
		return c.Quote{}, c.ErrConflict
	}
	return quote, nil
}
func (m *Module) SubmitInquiry(ctx context.Context, tx pgx.Tx, tenantID string, in c.InquiryInput) (c.Inquiry, error) {
	return service.SubmitInquiry(ctx, tx, tenantID, in)
}
