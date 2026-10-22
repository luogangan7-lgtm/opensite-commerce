// Package service validates shipment commands and calls the controlled SQL port.
// Dependencies: contract, private store and sqlc; callers: module composition.
// Invariants: all quantity/state changes are atomic in the caller transaction;
// shipments allocate stock committed by payment, never commit it again.
package service

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment/internal/store"
)

type Service struct{}

func (Service) CreateShipment(ctx context.Context, tx pgx.Tx, tenant string, in contract.CreateInput) (contract.Shipment, error) {
	if err := in.Validate(); err != nil {
		return contract.Shipment{}, err
	}
	q, err := store.Tenant(ctx, tx, tenant)
	if err != nil {
		return contract.Shipment{}, err
	}
	lines, _ := json.Marshal(in.Lines)
	raw, err := q.CreateFulfillmentShipment(ctx, db.CreateFulfillmentShipmentParams{OrderID: store.UUID(in.OrderID), ExpectedVersion: in.ExpectedVersion, IdempotencyKey: in.IdempotencyKey, Carrier: in.Carrier, TrackingNumber: in.TrackingNumber, Lines: lines})
	if err != nil {
		return contract.Shipment{}, store.MapError(err)
	}
	var out contract.Shipment
	if json.Unmarshal(raw, &out) != nil || !contract.ValidID(out.ID) || out.OrderID != in.OrderID || out.OrderVersion <= 0 || out.Status == "" {
		return contract.Shipment{}, contract.ErrUnavailable
	}
	return out, nil
}
