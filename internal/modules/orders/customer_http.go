// Responsibility: customer-owned sealed order history; dependencies: sqlc reads,
// customer actor contract and strict HTTP DTOs; caller: orders Routes/bootstrap.
// Invariants: live customer admission, SQL ownership on every read, read-only
// tenant snapshot, no contact/snapshot/provenance/staff adjustment fields.
package orders

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	g "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
	"github.com/oapi-codegen/nullable"
)

func historyActor(ctx context.Context, tenant uuid.UUID) (pgtype.UUID, error) {
	a, ok := c.ActorFromContext(ctx)
	id, err := uuid.Parse(a.CustomerID)
	if !ok || err != nil || id == uuid.Nil || a.TenantID != tenant.String() || a.SessionID == "" {
		return pgtype.UUID{}, apperrors.New(401, "unauthenticated", "Customer authentication is required")
	}
	return pgID(id), nil
}
func customerSummary(id pgtype.UUID, number int64, state, currency string, total int64, at time.Time) g.CustomerOrderSummary {
	return g.CustomerOrderSummary{Id: uuid.UUID(id.Bytes), OrderNumber: number, State: g.OrderState(state), Currency: currency, TotalMinor: minor(total), CreatedAt: at.UTC()}
}
func (h *HTTP) ListCustomerOrders(ctx context.Context, r g.ListCustomerOrdersRequestObject) (g.ListCustomerOrdersResponseObject, error) {
	actor, err := historyActor(ctx, r.TenantId)
	if err != nil {
		return nil, err
	}
	limit := 25
	if r.Params.Limit != nil {
		limit = *r.Params.Limit
	}
	if limit < 1 || limit > 100 {
		return nil, apperrors.New(400, "invalid_limit", "Limit must be between 1 and 100")
	}
	cursor, has, err := parseOrderCursor(r.Params.Cursor, "")
	if err != nil {
		return nil, err
	}
	page := g.CustomerOrderPage{Items: []g.CustomerOrderSummary{}}
	err = h.snapshot(ctx, r.TenantId, func(q *db.Queries) error {
		rows, e := q.ListCustomerOrders(ctx, db.ListCustomerOrdersParams{TenantID: pgID(r.TenantId), CustomerID: actor, HasCursor: has, CursorCreatedAt: pgtype.Timestamptz{Time: cursor.Time, Valid: true}, CursorID: pgID(cursor.ID), PageSize: int32(limit + 1)})
		if e != nil {
			return e
		}
		more := len(rows) > limit
		if more {
			rows = rows[:limit]
		}
		for _, o := range rows {
			page.Items = append(page.Items, customerSummary(o.ID, o.OrderNumber, o.State, o.Currency, o.TotalMinor, o.CreatedAt.Time))
		}
		if more {
			last := rows[len(rows)-1]
			next := encodeOrderCursor(orderCursor{Time: last.CreatedAt.Time, ID: uuid.UUID(last.ID.Bytes)})
			page.NextCursor = &next
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return g.ListCustomerOrders200JSONResponse{Body: page, Headers: g.ListCustomerOrders200ResponseHeaders{CacheControl: "no-store"}}, nil
}
func (h *HTTP) GetCustomerOrder(ctx context.Context, r g.GetCustomerOrderRequestObject) (g.GetCustomerOrderResponseObject, error) {
	actor, err := historyActor(ctx, r.TenantId)
	if err != nil {
		return nil, err
	}
	var detail g.CustomerOrderDetail
	err = h.snapshot(ctx, r.TenantId, func(q *db.Queries) error {
		o, e := q.GetCustomerOrder(ctx, db.GetCustomerOrderParams{TenantID: pgID(r.TenantId), CustomerID: actor, ID: pgID(r.OrderId)})
		if e != nil {
			return e
		}
		lines, e := q.ListCustomerOrderLines(ctx, db.ListCustomerOrderLinesParams{TenantID: pgID(r.TenantId), CustomerID: actor, OrderID: pgID(r.OrderId)})
		if e != nil {
			return e
		}
		detail = g.CustomerOrderDetail{Order: customerSummary(o.ID, o.OrderNumber, o.State, o.Currency, o.TotalMinor, o.CreatedAt.Time), ShippingMinor: minor(o.ShippingMinor), TaxMinor: minor(o.TaxMinor), Lines: []g.OrderLine{}}
		for _, l := range lines {
			detail.Lines = append(detail.Lines, g.OrderLine{LineId: uuid.UUID(l.ID.Bytes), SkuCode: l.Sku, Title: nullable.NewNullNullable[string](), Quantity: l.Quantity, UnitPriceMinor: minor(l.UnitPriceMinor), TaxMinor: minor(l.LineTaxMinor), LineTotalMinor: minor(l.LineTotalMinor)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return g.GetCustomerOrder200JSONResponse{Body: detail, Headers: g.GetCustomerOrder200ResponseHeaders{CacheControl: "no-store"}}, nil
}
