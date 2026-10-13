// Service applies authority, version and idempotency rules to frozen quotes.
// Dependencies: private sqlc store, quote contract, pgx and stdlib; callers: module.
// Invariants: authority is rechecked before history replay; all writes use the caller's transaction.
package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/internal/store"
	"math"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"
)

func guard(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor) error {
	if !a.Valid() {
		return c.ErrUnauthorized
	}
	if tx == nil {
		return c.ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
func Get(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, id string) (c.Quote, error) {
	if e := guard(ctx, tx, a); e != nil {
		return c.Quote{}, e
	}
	if !c.ValidID(id) {
		return c.Quote{}, c.ErrInvalid
	}
	q, e := store.Get(ctx, tx, a, id)
	if e != nil {
		return c.Quote{}, e
	}
	if a.Kind() == "customer" && q.CompanyID != a.CompanyID() {
		return c.Quote{}, c.ErrNotFound
	}
	return q, nil
}
func options(o c.WriteOptions) error {
	if o.IfMatch == "" {
		return c.ErrPreconditionRequired
	}
	if strings.TrimSpace(o.IdempotencyKey) == "" || len(o.IdempotencyKey) > 128 || strings.ContainsRune(o.IdempotencyKey, 0) || !utf8.ValidString(o.IdempotencyKey) {
		return c.ErrInvalid
	}
	return nil
}
func hash(a c.AuthorizedActor, op, target string, o c.WriteOptions, payload any) ([]byte, error) {
	data, e := json.Marshal(struct {
		Tenant, Kind, Actor, Company, Operation, Target, IfMatch string
		Payload                                                  any
	}{a.TenantID(), a.Kind(), a.ID(), a.CompanyID(), op, target, o.IfMatch, payload})
	if e != nil {
		return nil, c.ErrInvalid
	}
	h := sha256.Sum256(data)
	return h[:], nil
}
func claim(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, op, target string, o c.WriteOptions, payload any) (token string, replay *c.Quote, err error) {
	h, e := hash(a, op, target, o, payload)
	if e != nil {
		return "", nil, e
	}
	token = uuid.NewString()
	record, e := store.Claim(ctx, tx, a, op, o.IdempotencyKey, h, token)
	if e != nil {
		return "", nil, e
	}
	if !bytes.Equal(record.Hash, h) {
		return "", nil, c.ErrIdempotencyReused
	}
	if record.State == "completed" {
		var q c.Quote
		if json.Unmarshal(record.Response, &q) != nil || !c.ValidID(q.ID) || q.TenantID != a.TenantID() {
			return "", nil, c.ErrUnavailable
		}
		// PostgreSQL and JSON may attach different Location pointers to the same instant.
		// Canonical UTC makes fresh results and durable replays identical across hosts.
		q.ExpiresAt = q.ExpiresAt.UTC()
		if _, e = Get(ctx, tx, a, q.ID); e != nil {
			return "", nil, e
		}
		return "", &q, nil
	}
	if record.State != "in_progress" || record.Token != token {
		return "", nil, c.ErrConflict
	}
	return token, nil, nil
}
func finish(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, op string, o c.WriteOptions, token string, q c.Quote) error {
	data, e := json.Marshal(q)
	if e != nil || len(data) > 64*1024 {
		return c.ErrInvalid
	}
	return store.Complete(ctx, tx, a, op, o.IdempotencyKey, token, data)
}
func Create(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, in c.CreateInput, o c.WriteOptions, at time.Time) (c.Quote, error) {
	if e := guard(ctx, tx, a); e != nil {
		return c.Quote{}, e
	}
	if a.Kind() != "staff" {
		return c.Quote{}, c.ErrForbidden
	}
	if e := options(o); e != nil {
		return c.Quote{}, e
	}
	if o.IfMatch != "*" {
		return c.Quote{}, c.ErrPrecondition
	}
	if !c.ValidID(in.CompanyID) {
		return c.Quote{}, c.ErrInvalid
	}
	if e := store.Access(ctx, tx, a, in.CompanyID, "create"); e != nil {
		return c.Quote{}, e
	}
	token, replay, e := claim(ctx, tx, a, "quotes.create", "", o, in)
	if e != nil {
		return c.Quote{}, e
	}
	if replay != nil {
		return *replay, nil
	}
	rules, e := store.Rules(ctx, tx, a.TenantID(), in.ShippingProfileID)
	if e != nil {
		return c.Quote{}, e
	}
	snap, e := Calculate(in, rules, at)
	if e != nil {
		return c.Quote{}, e
	}
	id, e := store.Create(ctx, tx, a, in, snap)
	if e != nil {
		return c.Quote{}, e
	}
	q, e := Get(ctx, tx, a, id)
	if e != nil {
		return c.Quote{}, e
	}
	if e = finish(ctx, tx, a, "quotes.create", o, token, q); e != nil {
		return c.Quote{}, e
	}
	return q, nil
}
func Transition(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, id string, action c.Action, o c.WriteOptions, at time.Time) (c.Quote, error) {
	if e := guard(ctx, tx, a); e != nil {
		return c.Quote{}, e
	}
	if e := options(o); e != nil {
		return c.Quote{}, e
	}
	if !c.ValidID(id) {
		return c.Quote{}, c.ErrInvalid
	}
	switch action {
	case c.Send, c.Accept, c.Approve, c.Reject, c.Withdraw, c.Expire:
	default:
		return c.Quote{}, c.ErrInvalid
	}
	q, e := Get(ctx, tx, a, id)
	if e != nil {
		return c.Quote{}, e
	}
	if e = store.Access(ctx, tx, a, q.CompanyID, string(action)); e != nil {
		return c.Quote{}, e
	}
	op := "quotes." + string(action)
	token, replay, e := claim(ctx, tx, a, op, id, o, struct{ Action c.Action }{action})
	if e != nil {
		return c.Quote{}, e
	}
	if replay != nil {
		return *replay, nil
	}
	if o.IfMatch != q.ETag() {
		return c.Quote{}, c.ErrPrecondition
	}
	if e = ValidateTransition(q, action, a.Kind(), a.ID(), at); e != nil {
		return c.Quote{}, e
	}
	if e = store.Transition(ctx, tx, a, id, q.Version, action); e != nil {
		return c.Quote{}, e
	}
	q, e = Get(ctx, tx, a, id)
	if e != nil {
		return c.Quote{}, e
	}
	if e = finish(ctx, tx, a, op, o, token, q); e != nil {
		return c.Quote{}, e
	}
	return q, nil
}
func SubmitInquiry(ctx context.Context, tx pgx.Tx, tenantID string, in c.InquiryInput) (c.Inquiry, error) {
	if tx == nil {
		return c.Inquiry{}, c.ErrUnavailable
	}
	if e := ctx.Err(); e != nil {
		return c.Inquiry{}, e
	}
	if (in.ProductID != "" && !c.ValidID(in.ProductID)) || (in.Quantity != nil && (*in.Quantity <= 0 || math.IsNaN(*in.Quantity) || math.IsInf(*in.Quantity, 0))) {
		return c.Inquiry{}, c.ErrInvalid
	}
	if !c.ValidID(tenantID) || !c.ValidID(in.SiteID) || utf8.RuneCountInString(in.Name) > 200 || strings.TrimSpace(in.Message) == "" || utf8.RuneCountInString(in.Message) > 10000 || len(in.Email) > 254 || strings.ContainsRune(in.Name+in.Email+in.Message, 0) || !utf8.ValidString(in.Name+in.Email+in.Message) {
		return c.Inquiry{}, c.ErrInvalid
	}
	address, e := mail.ParseAddress(in.Email)
	if e != nil || address.Address != in.Email || address.Name != "" {
		return c.Inquiry{}, c.ErrInvalid
	}
	return store.SubmitInquiry(ctx, tx, tenantID, in)
}
