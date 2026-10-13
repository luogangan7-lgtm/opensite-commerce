// Package store adapts generated quote queries over caller-owned pgx transactions.
// Dependencies: sqlc db, quote contracts, pgx; caller: private quote service.
// Tables: quotes/lines/approvals/inquiries and shared idempotency via owning queries.
// Invariants: no pool/lifecycle, no raw production SQL, sanitized stable errors.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/shipping"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func id(v string) pgtype.UUID { var x pgtype.UUID; _ = x.Scan(v); return x }
func actorKind(a c.AuthorizedActor) string {
	if a.Kind() == "staff" {
		return "user"
	}
	return a.Kind()
}
func Access(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, company, operation string) error {
	ok, e := db.New(tx).QuoteActorAccess(ctx, db.QuoteActorAccessParams{TenantID: id(a.TenantID()), BuyerCompanyID: id(company), ActorKind: a.Kind(), ActorID: id(a.ID()), Operation: operation})
	if e != nil {
		return MapError(e)
	}
	if !ok {
		return c.ErrForbidden
	}
	return nil
}
func Get(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, quoteID string) (c.Quote, error) {
	r, e := db.New(tx).GetQuote(ctx, db.GetQuoteParams{TenantID: id(a.TenantID()), QuoteID: id(quoteID), ActorKind: a.Kind(), ActorID: id(a.ID())})
	return quote(r, e)
}

// GetAccepted returns the approved, DB-clock-unexpired row that passed the check; no row is ErrNotFound.
func GetAccepted(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, quoteID string) (c.Quote, error) {
	r, e := db.New(tx).GetAcceptedQuote(ctx, db.GetAcceptedQuoteParams{TenantID: id(a.TenantID()), QuoteID: id(quoteID), ActorKind: a.Kind(), ActorID: id(a.ID())})
	return quote(r, e)
}
func quote(r db.AppQuote, e error) (c.Quote, error) {
	if e != nil {
		return c.Quote{}, MapError(e)
	}
	var snap c.Snapshot
	if json.Unmarshal(r.Snapshot, &snap) != nil {
		return c.Quote{}, c.ErrUnavailable
	}
	accepted := ""
	if r.AcceptedBy.Valid {
		accepted = r.AcceptedBy.String()
	}
	return c.Quote{ID: r.ID.String(), TenantID: r.TenantID.String(), SiteID: r.SiteID.String(), InquiryID: r.InquiryID.String(), CompanyID: r.BuyerCompanyID.String(), Status: r.Status, Version: r.Version, Currency: r.Currency, ExpiresAt: r.ExpiresAt.Time.UTC(), CreatedBy: r.CreatedBy.String(), AcceptedBy: accepted, Snapshot: snap}, nil
}
func Rules(ctx context.Context, tx pgx.Tx, tenant, profile string) (c.CalculationRules, error) {
	r, e := db.New(tx).GetQuoteTaxRules(ctx, id(tenant))
	if e != nil {
		return c.CalculationRules{}, MapError(e)
	}
	var out c.CalculationRules
	if json.Unmarshal(r.Rules, &out.Tax) != nil {
		return out, c.ErrUnavailable
	}
	out.IncludeTax = r.PricesIncludeTax
	s, e := db.New(tx).GetQuoteShippingRules(ctx, db.GetQuoteShippingRulesParams{TenantID: id(tenant), ProfileID: id(profile)})
	if e != nil {
		return out, MapError(e)
	}
	var rules struct {
		Zones []shipping.Zone `json:"zones"`
		Rates []shipping.Rate `json:"rates"`
	}
	if json.Unmarshal(s, &rules) != nil {
		return out, c.ErrUnavailable
	}
	out.ShippingZones = rules.Zones
	out.ShippingRates = rules.Rates
	return out, nil
}
func Create(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, in c.CreateInput, snap c.Snapshot) (string, error) {
	snapshot, e := json.Marshal(snap)
	if e != nil {
		return "", c.ErrInvalid
	}
	lines, e := json.Marshal(snap.Lines)
	if e != nil {
		return "", c.ErrInvalid
	}
	result, e := db.New(tx).CreateQuote(ctx, db.CreateQuoteParams{TenantID: id(a.TenantID()), SiteID: id(in.SiteID), InquiryID: id(in.InquiryID), BuyerCompanyID: id(in.CompanyID), StaffActorID: id(a.ID()), Currency: in.Currency, Subtotal: snap.Subtotal, Tax: snap.TaxAmount, Shipping: snap.ShippingAmount, Total: snap.Total, Snapshot: snapshot, ExpiresAt: pgtype.Timestamptz{Time: in.ExpiresAt, Valid: true}, Lines: lines})
	if e != nil {
		return "", MapError(e)
	}
	return result.String(), nil
}
func Transition(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, quoteID string, version int64, action c.Action) error {
	_, e := db.New(tx).TransitionQuote(ctx, db.TransitionQuoteParams{TenantID: id(a.TenantID()), QuoteID: id(quoteID), ExpectedVersion: version, Action: string(action), ActorKind: a.Kind(), ActorID: id(a.ID())})
	return MapError(e)
}

type Receipt struct {
	Hash         []byte
	State, Token string
	Response     []byte
}

func Claim(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, op, key string, hash []byte, token string) (Receipt, error) {
	r, e := db.New(tx).QuoteClaimIdempotency(ctx, db.QuoteClaimIdempotencyParams{TenantID: id(a.TenantID()), ActorKind: actorKind(a), ActorID: id(a.ID()), Operation: op, IdempotencyKey: key, RequestHash: hash, TtlSeconds: 1800, ClaimToken: id(token)})
	if e != nil {
		return Receipt{}, MapError(e)
	}
	return Receipt{Hash: r.RequestHash, State: r.State, Token: r.ClaimToken.String(), Response: r.ResponseBody}, nil
}
func Complete(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, op, key, token string, response []byte) error {
	status := int32(200)
	if op == "quotes.create" {
		status = 201
	}
	ok, e := db.New(tx).QuoteCompleteIdempotency(ctx, db.QuoteCompleteIdempotencyParams{TenantID: id(a.TenantID()), ActorKind: actorKind(a), ActorID: id(a.ID()), Operation: op, IdempotencyKey: key, ClaimToken: id(token), ResponseBody: response, ResponseStatus: status})
	if e != nil {
		return MapError(e)
	}
	if ok != 1 {
		return c.ErrConflict
	}
	return nil
}
func SubmitInquiry(ctx context.Context, tx pgx.Tx, tenant string, in c.InquiryInput) (c.Inquiry, error) {
	quantity := pgtype.Numeric{}
	if in.Quantity != nil {
		b, _ := json.Marshal(*in.Quantity)
		_ = quantity.Scan(string(b))
	}
	r, e := db.New(tx).SubmitInquiry(ctx, db.SubmitInquiryParams{TenantID: id(tenant), SiteID: id(in.SiteID), ProductID: id(in.ProductID), Email: in.Email, Name: pgtype.Text{String: in.Name, Valid: true}, Quantity: quantity, Message: in.Message})
	if e != nil {
		return c.Inquiry{}, MapError(e)
	}
	// The CMS 005 default "new" is internal workflow state; the public receipt only acknowledges receipt.
	if r.Status != "new" {
		return c.Inquiry{}, c.ErrUnavailable
	}
	return c.Inquiry{ID: r.ID.String(), SiteID: r.SiteID.String(), Status: "received"}, nil
}
func MapError(e error) error {
	if e == nil {
		return nil
	}
	if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
		return e
	}
	if errors.Is(e, pgx.ErrNoRows) {
		return c.ErrNotFound
	}
	var p *pgxadapter.PgError
	if errors.As(e, &p) {
		switch p.Code {
		case "42501":
			return c.ErrForbidden
		case "P0002", "23503":
			return c.ErrNotFound
		case "P0003":
			return c.ErrPrecondition
		case "23514", "23505", "22023":
			return c.ErrConflict
		case "22003":
			return c.ErrOverflow
		case "22P02", "22001":
			return c.ErrInvalid
		case "40001", "40P01":
			return c.ErrConflict
		}
	}
	return c.ErrUnavailable
}
