// Package service owns order commands. It depends on the public price/tax/
// shipping/money results and private generated store, and is called by static
// composition/checkout/verified payment consumers. Commands never begin or
// commit a transaction: callers must roll back every error; snapshots, stock
// and outbox become durable together in the authorized tenant transaction.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/internal/store"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

type Service struct{}

var _ contract.Service = Service{}
var _ contract.QuoteCreator = Service{}
var _ contract.ReservationValidator = Service{}

func (Service) Create(ctx context.Context, tx pgx.Tx, tenantID string, in contract.CreateInput) (contract.Order, error) {
	p, err := prepare(in)
	if err != nil {
		return contract.Order{}, err
	}
	return create(ctx, tx, tenantID, in, p)
}

func (Service) CreateFromQuote(ctx context.Context, tx pgx.Tx, tenantID string, in contract.CreateInput) (contract.Order, error) {
	p, err := prepareQuoted(in)
	if err != nil {
		return contract.Order{}, err
	}
	return create(ctx, tx, tenantID, in, p)
}

func create(ctx context.Context, tx pgx.Tx, tenantID string, in contract.CreateInput, p prepared) (contract.Order, error) {
	q, err := store.Tenant(ctx, tx, tenantID)
	if err != nil {
		return contract.Order{}, err
	}
	orderID, _ := store.UUID(in.ID)
	customer := pgtype.UUID{}
	if in.Buyer.CustomerID != "" {
		customer, _ = store.UUID(in.Buyer.CustomerID)
		p.guest = nil
	}
	hash, err := requestHash(in, p)
	if err != nil {
		return contract.Order{}, err
	}
	// Creation-key serialization and replay precede all reservation/stock locks.
	existing, err := q.CheckOrderCreation(ctx, db.CheckOrderCreationParams{ID: orderID, CreationKey: in.IdempotencyKey, RequestHash: hash})
	if err != nil {
		return contract.Order{}, store.MapError(err)
	}
	if len(existing) == 1 {
		return order(existing[0]), nil
	}
	var lines []lineSnapshot
	if err = json.Unmarshal(p.lines, &lines); err != nil {
		return contract.Order{}, contract.ErrInvalid
	}
	// Lock order: explicit reservation rows before any automatic reserve takes a
	// stock lock (as transition and expiry do), else a concurrent expiry of an
	// explicit hold on the same SKU deadlocks with this create.
	var explicit []pgtype.UUID
	for _, l := range lines {
		if l.ReservationID != "" {
			id, _ := store.UUID(l.ReservationID)
			explicit = append(explicit, id)
		}
	}
	if len(explicit) > 0 {
		if err = q.LockOrderReservations(ctx, db.LockOrderReservationsParams{OrderID: orderID, ReservationIds: explicit}); err != nil {
			return contract.Order{}, store.MapError(err)
		}
	}
	for _, i := range reservationOrder(lines) {
		if lines[i].ReservationID != "" {
			continue
		}
		sku, _ := store.UUID(lines[i].SKUID)
		// Hashes remain fixed-length even when the caller's key is 128 bytes.
		keyHash := sha256.Sum256([]byte(strings.ToLower(in.ID) + ":" + lines[i].ID + ":" + in.IdempotencyKey))
		reservation, err := q.ReserveOrderInventory(ctx, db.ReserveOrderInventoryParams{SkuID: sku, Quantity: lines[i].Quantity, OrderID: orderID, IdempotencyKey: "order:" + hex.EncodeToString(keyHash[:]), TtlSeconds: p.ttl})
		if err != nil {
			return contract.Order{}, store.MapError(err)
		}
		lines[i].ReservationID = uuid.UUID(reservation.ID.Bytes).String()
	}
	p.lines, err = json.Marshal(lines)
	if err != nil {
		return contract.Order{}, contract.ErrInvalid
	}
	row, err := q.CreateOrder(ctx, db.CreateOrderParams{ID: orderID, CreationKey: in.IdempotencyKey, RequestHash: hash, CustomerID: customer, GuestContact: p.guest, Currency: in.Currency, ShippingMinor: in.Shipping.Charge.Amount, ShippingSnapshot: p.shipping, Lines: p.lines, Adjustments: p.adjustments})
	if err != nil {
		return contract.Order{}, store.MapError(err)
	}
	return order(row), nil
}

// Request hashing precedes generated reservation IDs. Retrying an automatic
// reservation after payment therefore hashes the same immutable request even
// though the stored reservation is committed. Explicit IDs remain part of it.
func requestHash(in contract.CreateInput, p prepared) ([]byte, error) {
	body, err := json.Marshal(struct {
		ID, CustomerID, Currency            string
		Guest, Shipping, Lines, Adjustments json.RawMessage
		TTL                                 int32
	}{ID: strings.ToLower(in.ID), CustomerID: strings.ToLower(in.Buyer.CustomerID), Currency: in.Currency, Guest: p.guest, Shipping: p.shipping, Lines: p.lines, Adjustments: p.adjustments, TTL: p.ttl})
	if err != nil {
		return nil, contract.ErrInvalid
	}
	h := sha256.Sum256(body)
	return h[:], nil
}

func (Service) Transition(ctx context.Context, tx pgx.Tx, tenantID string, in contract.TransitionInput) (contract.Order, error) {
	if !validID(in.OrderID) || !validKey(in.EventKey) || in.RefundMinor < 0 {
		return contract.Order{}, contract.ErrInvalid
	}
	switch in.Target {
	case contract.Paid, contract.PartiallyFulfilled, contract.Fulfilled, contract.Cancelled:
		if in.RefundMinor != 0 {
			return contract.Order{}, contract.ErrInvalid
		}
	case contract.PartiallyRefunded, contract.Refunded:
		if in.RefundMinor <= 0 {
			return contract.Order{}, contract.ErrInvalid
		}
	default:
		return contract.Order{}, contract.ErrInvalid
	}
	q, err := store.Tenant(ctx, tx, tenantID)
	if err != nil {
		return contract.Order{}, err
	}
	id, _ := store.UUID(in.OrderID)
	// Database deduplication runs before transition validation. A replay of an
	// older trusted event must succeed after subsequent state changes as well.
	row, err := q.TransitionOrder(ctx, db.TransitionOrderParams{ID: id, Target: string(in.Target), EventKey: in.EventKey, RefundMinor: in.RefundMinor})
	if err != nil {
		return contract.Order{}, store.MapError(err)
	}
	return order(row), nil
}

func (Service) GetPayable(ctx context.Context, tx pgx.Tx, tenantID, orderID string) (contract.Payable, error) {
	if !validID(orderID) {
		return contract.Payable{}, contract.ErrInvalid
	}
	q, err := store.Tenant(ctx, tx, tenantID)
	if err != nil {
		return contract.Payable{}, err
	}
	tenant, _ := store.UUID(tenantID)
	id, _ := store.UUID(orderID)
	row, err := q.GetPayableOrder(ctx, db.GetPayableOrderParams{TenantID: tenant, ID: id})
	if err != nil {
		return contract.Payable{}, store.MapError(err)
	}
	if row.State != string(contract.PendingPayment) {
		return contract.Payable{}, contract.ErrConflict
	}
	amount := money.Money{Amount: row.TotalMinor, Currency: row.Currency}
	if err := amount.Validate(); err != nil || amount.Amount < 0 || row.Version <= 0 {
		return contract.Payable{}, contract.ErrUnavailable
	}
	canonicalTenant := uuid.UUID(tenant.Bytes).String()
	canonicalID := uuid.UUID(row.ID.Bytes).String()
	return contract.Payable{OrderID: canonicalID, Amount: amount, IntentKey: fmt.Sprintf("payable:v1:%s:%s:%s", canonicalTenant, canonicalID, strconv.FormatInt(row.Version, 10)), Version: row.Version}, nil
}
func (Service) ValidateReservations(ctx context.Context, tx pgx.Tx, tenantID, orderID string) error {
	if !validID(orderID) {
		return contract.ErrInvalid
	}
	q, err := store.Tenant(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	id, _ := store.UUID(orderID)
	return store.MapError(q.ValidatePaymentReservations(ctx, id))
}

func (Service) LockLiveReservations(ctx context.Context, tx pgx.Tx, tenantID, orderID string, minRemainingSeconds int32) error {
	if !validID(orderID) || minRemainingSeconds < 0 {
		return contract.ErrInvalid
	}
	q, err := store.Tenant(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	tenant, _ := store.UUID(tenantID)
	id, _ := store.UUID(orderID)
	ids, err := q.ListOrderReservationIDs(ctx, db.ListOrderReservationIDsParams{TenantID: tenant, OrderID: id})
	if err != nil {
		return store.MapError(err)
	}
	if len(ids) == 0 {
		return contract.ErrNotFound
	}
	// Same global lock order (reservation id) as create, transition and expiry.
	if err = q.LockOrderReservations(ctx, db.LockOrderReservationsParams{OrderID: id, ReservationIds: ids}); err != nil {
		return store.MapError(err)
	}
	n, err := q.CountLiveOrderReservations(ctx, db.CountLiveOrderReservationsParams{MinRemainingSeconds: minRemainingSeconds, TenantID: tenant, ReservationIds: ids})
	if err != nil {
		return store.MapError(err)
	}
	if n.Live != n.Total || n.Total != int64(len(ids)) {
		return contract.ErrReservationInactive
	}
	return nil
}
func order(row db.AppOrder) contract.Order {
	return contract.Order{ID: uuid.UUID(row.ID.Bytes).String(), TenantID: uuid.UUID(row.TenantID.Bytes).String(), Number: row.OrderNumber, Version: row.Version, State: contract.State(row.State), Total: money.Money{Amount: row.TotalMinor, Currency: row.Currency}}
}

func reservationOrder(lines []lineSnapshot) []int {
	indices := make([]int, len(lines))
	for i := range lines {
		indices[i] = i
	}
	sort.Slice(indices, func(a, b int) bool {
		left, right := lines[indices[a]], lines[indices[b]]
		if left.SKUID != right.SKUID {
			return left.SKUID < right.SKUID
		}
		return left.ID < right.ID
	})
	return indices
}
