// Package service owns checkout's durable orchestration across DB and Payment Port.
// Dependencies: private SQLC store, Orders/Ledger contracts and pure calculators.
// Called by trusted transport/composition. DB phases fence provider IO, which is
// always outside transactions; provider success is recovered using stable keys.
package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/internal/store"
	ledger "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	orders "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
	"net/mail"
	"strings"
	"unicode/utf8"
)

type Service struct{ config contract.Config }

// captureReservationMarginSeconds is the minimum hold lifetime left when a
// payment claim is taken, so expiry cannot race the following Capture call.
// ponytail: fixed margin; a real PSP with longer capture latency must raise it
// (or extend holds) before going live.
const captureReservationMarginSeconds = 120

var _ contract.Service = (*Service)(nil)
var _ contract.ExistingOrderService = (*Service)(nil)

func New(c contract.Config) *Service { return &Service{config: c} }
func (s *Service) available() bool {
	return s != nil && s.config.Run != nil && s.config.Orders != nil && s.config.Ledger != nil && s.config.Payment != nil
}
func key(s string) bool {
	return len(s) > 0 && len(s) <= 128 && !strings.ContainsFunc(s, func(c rune) bool { return c < '!' || c > '~' })
}
func digest(x any) []byte { b, _ := json.Marshal(x); h := sha256.Sum256(b); return h[:] }

// within reports valid UTF-8 of at most max characters (OpenAPI maxLength semantics).
func within(v string, max int) bool { return utf8.ValidString(v) && utf8.RuneCountInString(v) <= max }
func checkedInput(actor contract.AuthorizedActor, in contract.StartInput) error {
	if !actor.Valid() {
		return contract.ErrForbidden
	}
	if in.QuoteID != "" || !tenanttx.ValidTenantID(in.CartID) || !key(in.IdempotencyKey) {
		return contract.ErrInvalid
	}
	if actor.Kind() == "guest" {
		p, e := mail.ParseAddress(in.BuyerContact.Email)
		if e != nil || p.Address != in.BuyerContact.Email || len(in.BuyerContact.Email) > 254 || !within(in.BuyerContact.Name, 200) || !within(in.BuyerContact.Phone, 64) {
			return contract.ErrInvalid
		}
	} else if in.BuyerContact != (orders.Contact{}) {
		return contract.ErrInvalid
	}
	a := in.ShippingAddress
	// Limits mirror CheckoutShippingAddress maxLength in api/openapi.yaml (characters).
	for _, f := range []struct {
		v   string
		max int
	}{{a.Country, 2}, {a.Subdivision, 100}, {a.Address1, 500}, {a.Address2, 500}, {a.City, 200}, {a.PostalCode, 32}} {
		if !within(f.v, f.max) || strings.ContainsFunc(f.v, func(c rune) bool { return c < ' ' }) {
			return contract.ErrInvalid
		}
	}
	if strings.TrimSpace(a.Address1) == "" || strings.TrimSpace(a.City) == "" || strings.TrimSpace(a.PostalCode) == "" {
		return contract.ErrInvalid
	}
	if in.AcceptedTotal.AmountMinor <= 0 || (money.Money{Amount: in.AcceptedTotal.AmountMinor, Currency: in.AcceptedTotal.Currency}).Validate() != nil {
		return contract.ErrInvalid
	}
	return nil
}
func (s *Service) run(ctx context.Context, tenantID string, fn func(context.Context, pgx.Tx, *db.Queries) error) error {
	return s.config.Run(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q, e := store.Queries(ctx, tx, tenantID)
		if e != nil {
			return e
		}
		return fn(ctx, tx, q)
	})
}
func decodeResult(b []byte) (contract.Result, error) {
	var r contract.Result
	if len(b) == 0 || json.Unmarshal(b, &r) != nil {
		return r, contract.ErrUnavailable
	}
	return r, nil
}
func snapshot(row db.AppCheckoutSession, status payment.PaymentStatus) contract.Result {
	return contract.Result{CheckoutID: row.ID.String(), OrderID: row.OrderID.String(), State: row.Phase, AmountMinor: row.AmountMinor, Currency: row.Currency, OrderVersion: row.PayableVersion, Payment: contract.Intent{ProviderRef: row.ProviderRef, Status: status}}
}

// Preview uses only a read-only snapshot; it never reserves stock or creates an order.
func (s *Service) Preview(ctx context.Context, actor contract.AuthorizedActor, in contract.PreviewInput) (contract.Preview, error) {
	var out contract.Preview
	if !actor.Valid() || !tenanttx.ValidTenantID(in.CartID) || len(in.Country) != 2 || in.Country[0] < 'A' || in.Country[0] > 'Z' || in.Country[1] < 'A' || in.Country[1] > 'Z' || !within(in.Region, 100) || in.PostalCode == "" || !within(in.PostalCode, 32) || strings.ContainsFunc(in.PostalCode, func(c rune) bool { return c < ' ' }) {
		return out, contract.ErrInvalid
	}
	if s.config.Read == nil || s.config.SnapshotCart == nil || s.config.Rules == nil {
		return out, contract.ErrUnavailable
	}
	err := s.config.Read(ctx, actor.TenantID(), func(ctx context.Context, tx pgx.Tx) error {
		cart, err := s.config.SnapshotCart.Read(ctx, tx, actor, in.CartID)
		if err != nil {
			return err
		}
		address := contract.ShippingAddress{Country: in.Country, Subdivision: in.Region, PostalCode: in.PostalCode}
		rules, err := s.config.Rules.Load(ctx, tx, actor, cart, address)
		if err != nil {
			return err
		}
		quoted, err := calculate(cart, rules, address)
		if err != nil {
			return err
		}
		total, err := (money.Money{Amount: quoted.Net, Currency: rules.Currency}).Add(money.Money{Amount: quoted.Tax, Currency: rules.Currency})
		if err != nil {
			return err
		}
		total, err = total.Add(quoted.Shipping.Charge)
		if err != nil {
			return err
		}
		out = contract.Preview{SubtotalMinor: quoted.Net, TaxMinor: quoted.Tax, ShippingMinor: quoted.Shipping.Charge.Amount, TotalMinor: total.Amount, Currency: rules.Currency, ShippingOptions: []contract.ShippingOption{{RateID: quoted.Shipping.RateID, Method: quoted.Shipping.Method, AmountMinor: quoted.Shipping.Charge.Amount}}}
		return nil
	})
	return out, err
}

// Get returns only the checkout owned by the verified actor and cart capability.
func (s *Service) Get(ctx context.Context, actor contract.AuthorizedActor, checkoutID, cartID string) (contract.Result, error) {
	var out contract.Result
	if !actor.Valid() || !tenanttx.ValidTenantID(checkoutID) || !tenanttx.ValidTenantID(cartID) {
		return out, contract.ErrInvalid
	}
	if s.config.Read == nil {
		return out, contract.ErrUnavailable
	}
	err := s.config.Read(ctx, actor.TenantID(), func(ctx context.Context, tx pgx.Tx) error {
		q, err := store.Queries(ctx, tx, actor.TenantID())
		if err != nil {
			return err
		}
		row, err := q.GetCheckoutByIDSnapshot(ctx, db.GetCheckoutByIDSnapshotParams{TenantID: store.UUID(actor.TenantID()), ID: store.UUID(checkoutID)})
		if errors.Is(err, pgx.ErrNoRows) {
			return contract.ErrNotFound
		}
		if err != nil {
			return store.MapError(err)
		}
		if row.ActorKind != actor.Kind() || row.ActorID != actor.ID() || row.CartID.String() != cartID {
			return contract.ErrNotFound
		}
		saved := row.FinalResult
		if saved == nil {
			saved = row.StartResult
		}
		if saved != nil {
			out, err = decodeResult(saved)
			return err
		}
		return contract.ErrUnavailable
	})
	return out, err
}
func operation(row db.AppCheckoutSession, action string) payment.PaymentOperation {
	return payment.PaymentOperation{CreatePaymentIntent: payment.CreatePaymentIntent{TenantID: row.TenantID.String(), OrderID: row.OrderID.String(), Amount: money.Money{Amount: row.AmountMinor, Currency: row.Currency}, OrderVersion: row.PayableVersion, IdempotencyKey: idempotency.DerivedKey("checkout.provider.v1", row.ID.String(), action)}, ProviderRef: row.ProviderRef}
}
func update(ctx context.Context, q *db.Queries, row db.AppCheckoutSession) (db.AppCheckoutSession, error) {
	x, e := q.UpdateCheckout(ctx, db.UpdateCheckoutParams{TenantID: row.TenantID, ID: row.ID, Phase: row.Phase, ProviderRef: row.ProviderRef, StartResult: row.StartResult, FinalResult: row.FinalResult, ClaimKind: row.ClaimKind, ClaimKey: row.ClaimKey})
	return x, store.MapError(e)
}
func (s *Service) Start(ctx context.Context, actor contract.AuthorizedActor, in contract.StartInput) (contract.Result, error) {
	var row db.AppCheckoutSession
	var result contract.Result
	if e := checkedInput(actor, in); e != nil {
		return result, e
	}
	if !s.available() || s.config.Cart == nil || s.config.Rules == nil {
		return result, contract.ErrUnavailable
	}
	requestHash := digest(in)
	e := s.run(ctx, actor.TenantID(), func(ctx context.Context, tx pgx.Tx, q *db.Queries) error {
		if e := q.LockCheckoutStart(ctx, actor.TenantID()+":"+actor.Kind()+":"+actor.ID()+":"+in.IdempotencyKey); e != nil {
			return store.MapError(e)
		}
		existing, e := q.GetCheckoutByKey(ctx, db.GetCheckoutByKeyParams{TenantID: store.UUID(actor.TenantID()), ActorKind: actor.Kind(), ActorID: actor.ID(), RequestKey: in.IdempotencyKey})
		if e == nil {
			if !bytes.Equal(existing.RequestHash, requestHash) {
				return contract.ErrKeyReused
			}
			row = existing
			return nil
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return store.MapError(e)
		}
		cart, e := s.config.Cart.Read(ctx, tx, actor, in.CartID)
		if e != nil {
			return e
		}
		if cart.ID != in.CartID {
			return contract.ErrNotFound
		}
		rules, e := s.config.Rules.Load(ctx, tx, actor, cart, in.ShippingAddress)
		if e != nil {
			return e
		}
		if rules.TenantID != actor.TenantID() {
			return contract.ErrForbidden
		}
		id := uuid.NewString()
		oid := orderID(id)
		// Cart line identities belong to shopping intent. Orders require a new immutable
		// line identity per checkout, while retries reuse the stored order snapshot.
		lines := append([]contract.CartLine(nil), cart.Lines...)
		for i := range lines {
			lines[i].ID = uuid.NewSHA1(uuid.MustParse(oid), []byte(lines[i].ID)).String()
		}
		pricingCart := cart
		pricingCart.Lines = lines
		quoted, e := calculate(pricingCart, rules, in.ShippingAddress)
		if e != nil {
			return e
		}
		total, e := (money.Money{Amount: quoted.Net, Currency: rules.Currency}).Add(money.Money{Amount: quoted.Tax, Currency: rules.Currency})
		if e != nil {
			return e
		}
		total, e = total.Add(quoted.Shipping.Charge)
		if e != nil {
			return e
		}
		if in.AcceptedTotal.AmountMinor != total.Amount || in.AcceptedTotal.Currency != total.Currency {
			return contract.ErrTotalChanged
		}
		if e = preflight(ctx, q, db.AppCheckoutSession{TenantID: store.UUID(actor.TenantID()), OrderID: store.UUID(oid), Currency: total.Currency, AmountMinor: total.Amount, ItemsNetMinor: quoted.Net, TaxMinor: quoted.Tax, ShippingMinor: quoted.Shipping.Charge.Amount}); e != nil {
			return e
		}
		buyer := orders.Buyer{Guest: in.BuyerContact}
		if actor.Kind() == "customer" {
			buyer = orders.Buyer{CustomerID: actor.ID()}
		}
		created, e := s.config.Orders.Create(ctx, tx, actor.TenantID(), orders.CreateInput{ID: oid, IdempotencyKey: idempotency.DerivedKey("checkout.order", id), Buyer: buyer, Currency: rules.Currency, Lines: quoted.Lines, Shipping: quoted.Shipping})
		if e != nil {
			return e
		}
		payable, e := s.config.Orders.GetPayable(ctx, tx, actor.TenantID(), created.ID)
		if e != nil {
			return e
		}
		if payable.OrderID != oid || payable.Amount != total || payable.Amount.Amount <= 0 || payable.Version <= 0 {
			return contract.ErrConflict
		}
		// Existing immutable snapshot is also the durable consent receipt. Older
		// cart sessions lack this field and cannot dispatch a new capture.
		addressSnapshot, _ := json.Marshal(struct {
			contract.ShippingAddress
			AcceptedTotal contract.AcceptedTotal `json:"accepted_total"`
		}{in.ShippingAddress, in.AcceptedTotal})
		row, e = q.InsertCheckout(ctx, db.InsertCheckoutParams{SourceKind: "cart", AddressSnapshot: addressSnapshot, TenantID: store.UUID(actor.TenantID()), ID: store.UUID(id), ActorKind: actor.Kind(), ActorID: actor.ID(), RequestKey: in.IdempotencyKey, RequestHash: requestHash, CartID: store.UUID(cart.ID), CartVersion: pgtype.Int8{Int64: cart.Version, Valid: true}, OrderID: store.UUID(oid), Currency: total.Currency, AmountMinor: total.Amount, ItemsNetMinor: quoted.Net, TaxMinor: quoted.Tax, ShippingMinor: quoted.Shipping.Charge.Amount, PayableVersion: payable.Version})
		return store.MapError(e)
	})
	if e != nil {
		return result, e
	}
	return s.createIntent(ctx, row)
}

// AdoptOrder admits only the Orders-owned sealed payable for this authorized
// customer. The actor/key receipt is checked first so replay after payment keeps
// the original intent. It never reads Cart/Rules or recreates the order.
func (s *Service) AdoptOrder(ctx context.Context, actor contract.AuthorizedActor, orderID, requestKey string) (contract.Result, error) {
	var row db.AppCheckoutSession
	if !actor.Valid() || actor.Kind() != "customer" {
		return contract.Result{}, contract.ErrForbidden
	}
	if !tenanttx.ValidTenantID(orderID) || !key(requestKey) {
		return contract.Result{}, contract.ErrInvalid
	}
	if !s.available() {
		return contract.Result{}, contract.ErrUnavailable
	}
	requestHash := digest(struct{ Source, OrderID, Key string }{"existing_order", orderID, requestKey})
	e := s.run(ctx, actor.TenantID(), func(ctx context.Context, tx pgx.Tx, q *db.Queries) error {
		if e := q.LockCheckoutStart(ctx, actor.TenantID()+":"+actor.Kind()+":"+actor.ID()+":"+requestKey); e != nil {
			return store.MapError(e)
		}
		existing, e := q.GetCheckoutByKey(ctx, db.GetCheckoutByKeyParams{TenantID: store.UUID(actor.TenantID()), ActorKind: actor.Kind(), ActorID: actor.ID(), RequestKey: requestKey})
		if e == nil {
			if existing.SourceKind != "existing_order" || !bytes.Equal(existing.RequestHash, requestHash) {
				return contract.ErrKeyReused
			}
			row = existing
			return nil
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return store.MapError(e)
		}
		payable, e := s.config.Orders.GetPayable(ctx, tx, actor.TenantID(), orderID)
		if e != nil {
			// Neither a missing/foreign order nor its non-payable state is disclosed.
			if errors.Is(e, orders.ErrNotFound) || errors.Is(e, orders.ErrConflict) {
				return contract.ErrNotFound
			}
			return e
		}
		if payable.OrderID != orderID || payable.CustomerID != actor.ID() {
			return contract.ErrNotFound
		}
		if payable.Amount.Amount <= 0 || payable.Amount.Validate() != nil || payable.Version <= 0 || payable.ItemsNetMinor < 0 || payable.TaxMinor < 0 || payable.ShippingMinor < 0 {
			return contract.ErrConflict
		}
		if e = preflight(ctx, q, db.AppCheckoutSession{TenantID: store.UUID(actor.TenantID()), OrderID: store.UUID(orderID), Currency: payable.Amount.Currency, AmountMinor: payable.Amount.Amount, ItemsNetMinor: payable.ItemsNetMinor, TaxMinor: payable.TaxMinor, ShippingMinor: payable.ShippingMinor}); e != nil {
			return e
		}
		row, e = q.InsertCheckout(ctx, db.InsertCheckoutParams{SourceKind: "existing_order", AddressSnapshot: []byte("{}"), TenantID: store.UUID(actor.TenantID()), ID: store.UUID(uuid.NewString()), ActorKind: actor.Kind(), ActorID: actor.ID(), RequestKey: requestKey, RequestHash: requestHash, OrderID: store.UUID(orderID), Currency: payable.Amount.Currency, AmountMinor: payable.Amount.Amount, ItemsNetMinor: payable.ItemsNetMinor, TaxMinor: payable.TaxMinor, ShippingMinor: payable.ShippingMinor, PayableVersion: payable.Version})
		return store.MapError(e)
	})
	if e != nil {
		return contract.Result{}, e
	}
	return s.createIntent(ctx, row)
}

// createIntent is the one durable pending -> ready protocol for both sources.
// Admission has committed before provider IO; retries use the same session key.
func (s *Service) createIntent(ctx context.Context, row db.AppCheckoutSession) (contract.Result, error) {
	var result contract.Result
	if row.StartResult != nil {
		return decodeResult(row.StartResult)
	}
	if row.Phase == "failed" && row.FinalResult != nil {
		return decodeResult(row.FinalResult)
	}
	if row.Phase != "pending" {
		return result, contract.ErrConflict
	}
	// Original order/payable transaction is committed. No locks survive this call.
	intent, e := s.config.Payment.CreateIntent(ctx, operation(row, "create").CreatePaymentIntent)
	if e != nil {
		var pe *payment.PaymentError
		if errors.As(e, &pe) && pe.Code == payment.ErrorNonRetryable {
			return s.failCreation(ctx, row)
		}
		return result, e
	}
	if !validIntent(intent) || intent.Status != payment.StatusAuthorized && intent.Status != payment.StatusPending && intent.Status != payment.StatusRequiresAction {
		return result, contract.ErrUnavailable
	}
	e = s.run(ctx, row.TenantID.String(), func(ctx context.Context, tx pgx.Tx, q *db.Queries) error {
		current, e := q.GetCheckoutByID(ctx, db.GetCheckoutByIDParams{TenantID: row.TenantID, ID: row.ID})
		if e != nil {
			return store.MapError(e)
		}
		if current.StartResult != nil {
			result, e = decodeResult(current.StartResult)
			return e
		}
		if current.Phase != "pending" {
			return contract.ErrConflict
		}
		current.ProviderRef = intent.ProviderRef
		current.Phase = "ready"
		result = snapshot(current, intent.Status)
		result.Payment.NextActionKind = intent.NextAction.Kind
		result.Payment.NextActionURL = intent.NextAction.URL
		current.StartResult, _ = json.Marshal(result)
		_, e = update(ctx, q, current)
		return e
	})
	return result, e
}
func validIntent(i payment.PaymentIntentResult) bool {
	return strings.TrimSpace(i.ProviderRef) != "" && len(i.ProviderRef) <= 512 && !strings.ContainsAny(i.ProviderRef, "\r\n\x00") && len(i.NextAction.Kind) <= 64 && len(i.NextAction.URL) <= 2048
}

// preflight uses the same journal validation before either source authorizes
// provider IO; missing or mistyped ledger accounts fail before an intent exists.
func preflight(ctx context.Context, q *db.Queries, row db.AppCheckoutSession) error {
	accountRows, e := q.GetCheckoutLedgerAccounts(ctx, row.TenantID)
	if e != nil {
		return store.MapError(e)
	}
	configured := map[string]db.GetCheckoutLedgerAccountsRow{}
	for _, a := range accountRows {
		configured[a.Code] = a
	}
	_, e = journal(row, "preflight", configured)
	return e
}

// journal records accrual and settlement in one balanced journal. Zero-valued
// revenue/tax/shipping components are omitted; missing/type-wrong accounts refuse.
func journal(row db.AppCheckoutSession, event string, accounts map[string]db.GetCheckoutLedgerAccountsRow) (ledger.Transaction, error) {
	out := ledger.Transaction{TenantID: row.TenantID.String(), Currency: row.Currency, SourceType: "order", SourceID: row.OrderID.String(), Event: "checkout:" + hex.EncodeToString(digest(event))}
	add := func(code string, side ledger.Side, amount int64, kind string) error {
		if amount == 0 {
			return nil
		}
		a, ok := accounts[code]
		if !ok || a.AccountType != kind {
			return contract.ErrUnavailable
		}
		out.Entries = append(out.Entries, ledger.Entry{AccountID: a.ID.String(), Side: side, Amount: money.Money{Amount: amount, Currency: row.Currency}})
		return nil
	}
	for _, v := range []struct {
		code   string
		side   ledger.Side
		amount int64
		kind   string
	}{{"accounts_receivable", ledger.Debit, row.AmountMinor, "asset"}, {"revenue", ledger.Credit, row.ItemsNetMinor, "revenue"}, {"tax_payable", ledger.Credit, row.TaxMinor, "liability"}, {"shipping_revenue", ledger.Credit, row.ShippingMinor, "revenue"}, {"cash", ledger.Debit, row.AmountMinor, "asset"}, {"accounts_receivable", ledger.Credit, row.AmountMinor, "asset"}} {
		if e := add(v.code, v.side, v.amount, v.kind); e != nil {
			return out, e
		}
	}
	return out, out.Validate()
}

// Event digests bind every trusted provider field. Shape validation is only the
// last defense: HTTP must verify signatures/account-to-tenant BEFORE Confirm.
func (s *Service) Confirm(ctx context.Context, event payment.VerifiedEvent) (contract.Result, error) {
	var zero contract.Result
	if e := event.Validate(); e != nil {
		return zero, contract.ErrInvalid
	}
	if !s.available() {
		return zero, contract.ErrUnavailable
	}
	switch event.Status {
	case payment.StatusAuthorized, payment.StatusCaptured:
		return s.finish(ctx, contract.AuthorizedActor{}, event.TenantID, event.OrderID, "", "payment", event.EventID, digest(event), &event, "confirming", "paid")
	case payment.StatusFailed, payment.StatusCanceled:
		return s.finish(ctx, contract.AuthorizedActor{}, event.TenantID, event.OrderID, "", "payment", event.EventID, digest(event), &event, "cancelling", "failed")
	}
	return zero, contract.ErrInvalid
}
func (s *Service) Cancel(ctx context.Context, actor contract.AuthorizedActor, in contract.CancelInput) (contract.Result, error) {
	if !actor.Valid() {
		return contract.Result{}, contract.ErrForbidden
	}
	if !tenanttx.ValidTenantID(in.CheckoutID) || !key(in.IdempotencyKey) {
		return contract.Result{}, contract.ErrInvalid
	}
	if !s.available() {
		return contract.Result{}, contract.ErrUnavailable
	}
	requestHash := digest(struct{ CheckoutID, Key string }{in.CheckoutID, in.IdempotencyKey})
	return s.finish(ctx, actor, actor.TenantID(), "", in.CheckoutID, "cancel", idempotency.DerivedKey("checkout.cancel", actor.Kind(), actor.ID(), in.IdempotencyKey), requestHash, nil, "cancelling", "cancelled")
}
func (s *Service) finish(ctx context.Context, actor contract.AuthorizedActor, tenantID, oid, cid, kind, eventKey string, requestHash []byte, event *payment.VerifiedEvent, claimPhase, terminal string) (contract.Result, error) {
	if kind == "payment" {
		eventKey = idempotency.DerivedKey("checkout.payment", eventKey)
	}
	var row db.AppCheckoutSession
	var result contract.Result
	complete := false
	e := s.run(ctx, tenantID, func(ctx context.Context, tx pgx.Tx, q *db.Queries) error {
		var e error
		if oid != "" {
			row, e = q.GetCheckoutByOrder(ctx, db.GetCheckoutByOrderParams{TenantID: store.UUID(tenantID), OrderID: store.UUID(oid)})
		} else {
			row, e = q.GetCheckoutByID(ctx, db.GetCheckoutByIDParams{TenantID: store.UUID(tenantID), ID: store.UUID(cid)})
		}
		if e != nil {
			return store.MapError(e)
		}
		if actor.Valid() && (row.ActorKind != actor.Kind() || row.ActorID != actor.ID()) {
			return contract.ErrNotFound
		}
		if event != nil && (row.OrderID.String() != event.OrderID || row.Currency != event.Amount.Currency || row.AmountMinor != event.Amount.Amount || row.PayableVersion != event.OrderVersion || row.ProviderRef != event.ProviderRef) {
			return contract.ErrConflict
		}
		attempt, e := q.GetCheckoutAttempt(ctx, db.GetCheckoutAttemptParams{TenantID: row.TenantID, EventKind: kind, EventKey: eventKey})
		if e == nil {
			if attempt.CheckoutID != row.ID || !bytes.Equal(attempt.RequestHash, requestHash) {
				return contract.ErrKeyReused
			}
			if attempt.Result != nil {
				result, e = decodeResult(attempt.Result)
				complete = true
				return e
			}
		} else if !errors.Is(e, pgx.ErrNoRows) {
			return store.MapError(e)
		} else {
			_, e = q.InsertCheckoutAttempt(ctx, db.InsertCheckoutAttemptParams{TenantID: row.TenantID, EventKind: kind, EventKey: eventKey, CheckoutID: row.ID, RequestHash: requestHash})
			if e != nil {
				return store.MapError(e)
			}
		}
		if row.FinalResult != nil {
			// A failure/cancel callback (e.g. Fake's own canceled event after a
			// successful Cancel) agrees with any non-paid terminal; paid never does.
			if row.Phase != terminal && !(terminal == "failed" && row.Phase == "cancelled") {
				return contract.ErrConflict
			}
			result, e = decodeResult(row.FinalResult)
			if e != nil {
				return e
			}
			complete = true
			return store.MapError(q.CompleteCheckoutAttempt(ctx, db.CompleteCheckoutAttemptParams{TenantID: row.TenantID, EventKind: kind, EventKey: eventKey, Result: row.FinalResult}))
		}
		if row.Phase == "ready" {
			// Consent authorizes a new claim, never retroactively blocks recovery.
			if claimPhase == "confirming" && row.SourceKind == "cart" {
				var receipt struct {
					AcceptedTotal contract.AcceptedTotal `json:"accepted_total"`
				}
				if json.Unmarshal(row.AddressSnapshot, &receipt) != nil || receipt.AcceptedTotal.AmountMinor != row.AmountMinor || receipt.AcceptedTotal.Currency != row.Currency {
					return contract.ErrTotalChanged
				}
			}
			row.Phase = claimPhase
			row.ClaimKind = store.Text(kind)
			row.ClaimKey = store.Text(eventKey)
			if row, e = update(ctx, q, row); e != nil || claimPhase != "confirming" {
				return e
			}
			// Never capture unless every hold is live and locked now with a margin
			// covering provider IO; otherwise void the intent and cancel the order.
			e = s.config.Orders.LockLiveReservations(ctx, tx, tenantID, row.OrderID.String(), captureReservationMarginSeconds)
			if errors.Is(e, orders.ErrReservationInactive) {
				row.Phase = "failing"
				row, e = update(ctx, q, row)
			}
			return e
		}
		if (row.Phase != claimPhase && !(claimPhase == "confirming" && row.Phase == "failing")) || row.ClaimKind.String != kind || row.ClaimKey.String != eventKey {
			return contract.ErrConflict
		}
		if claimPhase == "confirming" && row.Phase == "confirming" {
			// Capture may already have succeeded before a lost response/finalize
			// rollback. Revalidate before replaying IO, but never cancel uncertain
			// money merely because a hold expired while recovery was pending.
			e = s.config.Orders.LockLiveReservations(ctx, tx, tenantID, row.OrderID.String(), captureReservationMarginSeconds)
			if errors.Is(e, orders.ErrReservationInactive) {
				return contract.ErrRetry
			}
			return e
		}
		return nil
	})
	if e != nil || complete {
		return result, e
	}
	// This DB claim is durable before provider IO. Retry retains this exact payable
	// and operation key even after Capture succeeded but finalize rolled back.
	var intent payment.PaymentIntentResult
	if claimPhase == "confirming" && row.Phase != "failing" {
		intent, e = s.config.Payment.Capture(ctx, operation(row, "capture"))
		if e != nil {
			var pe *payment.PaymentError
			if !errors.As(e, &pe) || pe.Code != payment.ErrorNonRetryable {
				return result, e
			}
			// A permanent Fake rejection occurs before capture. Durably choose failure
			// recovery before Cancel; an adapter with ambiguous effects must be retryable.
			e = s.run(ctx, tenantID, func(ctx context.Context, tx pgx.Tx, q *db.Queries) error {
				current, err := q.GetCheckoutByID(ctx, db.GetCheckoutByIDParams{TenantID: row.TenantID, ID: row.ID})
				if err != nil {
					return store.MapError(err)
				}
				if current.Phase == "failing" {
					row = current
					return nil
				}
				if current.Phase != "confirming" || current.ClaimKind.String != kind || current.ClaimKey.String != eventKey {
					return contract.ErrConflict
				}
				current.Phase = "failing"
				row, err = update(ctx, q, current)
				return err
			})
			if e != nil {
				return result, e
			}
		}
	}
	if claimPhase == "cancelling" || row.Phase == "failing" {
		intent, e = s.config.Payment.Cancel(ctx, operation(row, "cancel"))
		if e != nil {
			return result, e
		}
	}
	if row.Phase == "failing" {
		claimPhase = "failing"
		terminal = "failed"
	}
	expected := payment.StatusCaptured
	if claimPhase == "cancelling" || claimPhase == "failing" {
		expected = payment.StatusCanceled
	}
	if !validIntent(intent) || intent.ProviderRef != row.ProviderRef || intent.Status != expected {
		return result, contract.ErrUnavailable
	}
	e = s.run(ctx, tenantID, func(ctx context.Context, tx pgx.Tx, q *db.Queries) error {
		current, e := q.GetCheckoutByID(ctx, db.GetCheckoutByIDParams{TenantID: row.TenantID, ID: row.ID})
		if e != nil {
			return store.MapError(e)
		}
		if current.FinalResult != nil {
			result, e = decodeResult(current.FinalResult)
			return e
		}
		if current.Phase != claimPhase || current.ClaimKind.String != kind || current.ClaimKey.String != eventKey {
			return contract.ErrConflict
		}
		target := orders.Cancelled
		if terminal == "paid" {
			target = orders.Paid
		}
		transitionKey := "checkout:" + current.ID.String() + ":" + terminal
		changed, e := s.config.Orders.Transition(ctx, tx, tenantID, orders.TransitionInput{OrderID: current.OrderID.String(), Target: target, EventKey: transitionKey})
		if e != nil {
			return e
		}
		if changed.State != target {
			return contract.ErrConflict
		}
		if terminal == "paid" {
			rows, e := q.GetCheckoutLedgerAccounts(ctx, current.TenantID)
			if e != nil {
				return store.MapError(e)
			}
			accounts := map[string]db.GetCheckoutLedgerAccountsRow{}
			for _, a := range rows {
				accounts[a.Code] = a
			}
			posting, e := journal(current, event.EventID, accounts)
			if e != nil {
				return e
			}
			if _, e = s.config.Ledger.Post(ctx, tx, posting); e != nil {
				return e
			}
		}
		current.Phase = terminal
		result = snapshot(current, intent.Status)
		current.FinalResult, _ = json.Marshal(result)
		if _, e = update(ctx, q, current); e != nil {
			return e
		}
		return store.MapError(q.CompleteCheckoutAttempt(ctx, db.CompleteCheckoutAttemptParams{TenantID: current.TenantID, EventKind: kind, EventKey: eventKey, Result: current.FinalResult}))
	})
	return result, e
}

// failCreation is used only for a definitive Fake Create rejection. Retryable /
// timeout outcomes retain pending and retry the same original provider key.
func (s *Service) failCreation(ctx context.Context, row db.AppCheckoutSession) (contract.Result, error) {
	var result contract.Result
	e := s.run(ctx, row.TenantID.String(), func(ctx context.Context, tx pgx.Tx, q *db.Queries) error {
		current, e := q.GetCheckoutByID(ctx, db.GetCheckoutByIDParams{TenantID: row.TenantID, ID: row.ID})
		if e != nil {
			return store.MapError(e)
		}
		if current.FinalResult != nil {
			returnValue, err := decodeResult(current.FinalResult)
			result = returnValue
			return err
		}
		if current.Phase != "pending" {
			return contract.ErrConflict
		}
		changed, e := s.config.Orders.Transition(ctx, tx, current.TenantID.String(), orders.TransitionInput{OrderID: current.OrderID.String(), Target: orders.Cancelled, EventKey: idempotency.DerivedKey("checkout.order-transition.v1", current.ID.String(), "create-failure")})
		if e != nil {
			return e
		}
		if changed.State != orders.Cancelled {
			return contract.ErrConflict
		}
		current.Phase = "failed"
		current.ClaimKind = store.Text("create_failure")
		current.ClaimKey = store.Text(current.RequestKey)
		result = snapshot(current, payment.StatusFailed)
		current.FinalResult, _ = json.Marshal(result)
		_, e = update(ctx, q, current)
		return e
	})
	return result, e
}
