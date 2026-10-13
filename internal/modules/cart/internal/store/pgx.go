// Package store persists cart intent through generated SQL. It depends on db,
// pgx error types and the public cart/pricing contracts; cart.Module calls it.
// Every operation runs inside the platform tenant runner's transaction with a
// transaction-local runtime role and actor, with no pooled actor leakage.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
)

// Run executes fn in one tenant-scoped transaction opened by the platform
// runner (pgxadapter.QueryRunner): the store never holds a pool or begins a
// transaction itself, so tenanttx.Run always scopes app.tenant_id first.
type Run func(context.Context, string, func(*db.Queries) error) error

// Store writes through write and reads through read (a read-only snapshot).
type Store struct{ write, read Run }

func New(write, read Run) *Store { return &Store{write: write, read: read} }

// in narrows the runner's transaction to the cart role and actor before fn.
// Failures before fn runs mean no usable transaction, so they surface as the
// declared 503; authorization and business errors pass through unchanged.
func (s *Store) in(ctx context.Context, actor contract.AuthorizedActor, read bool, fn func(*db.Queries) error) error {
	run := s.write
	if read {
		run = s.read
	}
	if run == nil {
		return unavailable()
	}
	entered := false
	err := run(ctx, actor.TenantID, func(q *db.Queries) error {
		entered = true
		if err := q.SetCartRuntimeRole(ctx); err != nil {
			return err
		}
		if err := q.SetCartContext(ctx, db.SetCartContextParams{TenantContext: actor.TenantID, CustomerContext: actor.CustomerID, TokenContext: hex.EncodeToString(actor.GuestTokenHash)}); err != nil {
			return err
		}
		return fn(q)
	})
	var failure *apperrors.Error
	if err != nil && !entered && !errors.As(err, &failure) {
		return unavailable()
	}
	return err
}

func (s *Store) Create(ctx context.Context, actor contract.AuthorizedActor, currency string) (snapshot contract.Snapshot, err error) {
	var bytes [16]byte
	if _, err = rand.Read(bytes[:]); err != nil {
		return contract.Snapshot{}, err
	}
	bytes[6] = (bytes[6] & 15) | 64
	bytes[8] = (bytes[8] & 63) | 128
	err = s.in(ctx, actor, false, func(q *db.Queries) error {
		row, err := q.CreateCart(ctx, db.CreateCartParams{TenantID: uuid(actor.TenantID), ID: pgtype.UUID{Bytes: bytes, Valid: true}, CustomerID: uuid(actor.CustomerID), GuestTokenHash: actor.GuestTokenHash, Currency: currency})
		if err != nil {
			return hidden(err)
		}
		snapshot, err = project(ctx, q, row, time.Now().UTC())
		return err
	})
	return snapshot, err
}

func (s *Store) Read(ctx context.Context, actor contract.AuthorizedActor, id string) (snapshot contract.Snapshot, err error) {
	err = s.in(ctx, actor, true, func(q *db.Queries) error {
		row, err := q.GetCart(ctx, db.GetCartParams{TenantID: uuid(actor.TenantID), ID: uuid(id)})
		if errors.Is(err, pgx.ErrNoRows) {
			return missing()
		}
		if err != nil {
			return err
		}
		snapshot, err = project(ctx, q, row, time.Now().UTC())
		return err
	})
	return snapshot, err
}

func (s *Store) Change(ctx context.Context, actor contract.AuthorizedActor, id, sku string, quantity int64, check, validate func(contract.Snapshot) error) (after contract.Snapshot, err error) {
	err = s.in(ctx, actor, false, func(q *db.Queries) error {
		row, err := q.GetCartForUpdate(ctx, db.GetCartForUpdateParams{TenantID: uuid(actor.TenantID), ID: uuid(id)})
		if errors.Is(err, pgx.ErrNoRows) {
			return missing()
		}
		if err != nil {
			return err
		}
		at := time.Now().UTC()
		before, err := project(ctx, q, row, at)
		if err != nil {
			return err
		}
		if err = check(before); err != nil {
			return err
		}
		if quantity == 0 {
			err = q.DeleteCartLine(ctx, db.DeleteCartLineParams{TenantID: row.TenantID, CartID: row.ID, SkuID: uuid(sku)})
		} else {
			_, err = q.GetActiveCartSKU(ctx, db.GetActiveCartSKUParams{TenantID: row.TenantID, SkuID: uuid(sku)})
			if errors.Is(err, pgx.ErrNoRows) {
				return apperrors.New(422, "sku_not_active", "SKU must exist and be active")
			}
			if err == nil {
				err = q.SetCartLine(ctx, db.SetCartLineParams{TenantID: row.TenantID, CartID: row.ID, SkuID: uuid(sku), Quantity: quantity})
			}
		}
		if err != nil {
			return hidden(err)
		}
		row, err = q.AdvanceCartVersion(ctx, db.AdvanceCartVersionParams{TenantID: row.TenantID, ID: row.ID, ExpectedVersion: row.Version})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperrors.New(412, "precondition_failed", "Cart changed")
		}
		if err != nil {
			return hidden(err)
		}
		if after, err = project(ctx, q, row, at); err != nil {
			return err
		}
		return validate(after)
	})
	return after, err
}

// hidden maps an RLS/privilege denial on a cart write to the ownership-hiding
// miss: the tenant or customer became inactive after the cart row was locked.
func hidden(err error) error {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && (pgerr.Code == "42501" || pgerr.Code == "23503") {
		return missing()
	}
	return err
}

// Four batched reads per representation, independent of cart line count.
func project(ctx context.Context, q *db.Queries, row db.AppCart, at time.Time) (contract.Snapshot, error) {
	s := contract.Snapshot{ID: id(row.ID), TenantID: id(row.TenantID), Currency: row.Currency, Version: row.Version, At: at}
	lines, err := q.ListCartLines(ctx, db.ListCartLinesParams{TenantID: row.TenantID, CartID: row.ID})
	if err != nil {
		return s, err
	}
	stocks, err := q.CartAvailability(ctx, db.CartAvailabilityParams{TenantID: row.TenantID, CartID: row.ID})
	if err != nil {
		return s, err
	}
	available := make(map[string]int64, len(stocks))
	for _, stock := range stocks {
		available[id(stock.SkuID)] = stock.Available
	}
	for _, line := range lines {
		s.Lines = append(s.Lines, contract.StoredLine{SKUID: id(line.SkuID), Quantity: line.Quantity, AvailableQuantity: available[id(line.SkuID)], Active: line.LifecycleStatus == "ready"})
	}
	prices, err := q.ListCartPriceEntries(ctx, db.ListCartPriceEntriesParams{TenantID: row.TenantID, CartID: row.ID, Currency: row.Currency, AtTime: pgtype.Timestamptz{Time: at, Valid: true}})
	if err != nil {
		return s, err
	}
	indices := map[string]int{}
	for _, entry := range prices {
		listID := id(entry.PriceListID)
		index, exists := indices[listID]
		if !exists {
			list := pricing.PriceList{TenantID: id(entry.TenantID), ID: listID, Currency: entry.Currency, Status: entry.Status, Priority: entry.Priority, StartsAt: entry.StartsAt.Time, Audience: entry.Audience, CustomerGroupID: id(entry.CustomerGroupID), BuyerCompanyID: id(entry.BuyerCompanyID)}
			if entry.EndsAt.Valid {
				end := entry.EndsAt.Time
				list.EndsAt = &end
			}
			index = len(s.PriceLists)
			indices[listID] = index
			s.PriceLists = append(s.PriceLists, list)
		}
		s.PriceLists[index].Entries = append(s.PriceLists[index].Entries, pricing.Entry{SKUID: id(entry.SkuID), MinQuantity: entry.MinQuantity, UnitAmount: entry.UnitAmount})
	}
	return s, nil
}

func uuid(value string) pgtype.UUID {
	var u pgtype.UUID
	if value != "" {
		_ = u.Scan(value)
	}
	return u
}
func id(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
func missing() error { return apperrors.New(404, "cart_not_found", "Cart not found") }
func unavailable() error {
	return apperrors.New(503, "cart_unavailable", "Cart storage is unavailable")
}
