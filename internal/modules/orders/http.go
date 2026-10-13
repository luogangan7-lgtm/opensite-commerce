// Responsibility: admin read-only HTTP projection of sealed order snapshots
// (listOrders/getOrder). Dependencies: generated sqlc reads, platform strict
// httpapi DTOs and authorization; caller: API bootstrap (Config.Read is the
// tenant runner's read-only snapshot transaction). Invariants: tenant comes only
// from router authorization, guest contact never leaves the database, money is
// serialized as decimal strings, cursors are canonical and bound to the filter.
package orders

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/oapi-codegen/nullable"
)

// HTTPConfig.Read must run fn in a read-only tenant transaction
// (pgxadapter.QueryRunner.RunAuthorizedSnapshot); a nil Read answers 503.
type HTTPConfig struct {
	Read func(context.Context, string, func(*db.Queries) error) error
}
type HTTP struct {
	httpapi.StrictServerInterface
	read func(context.Context, string, func(*db.Queries) error) error
}

func NewHTTP(config HTTPConfig) *HTTP { return &HTTP{read: config.Read} }

func (h *HTTP) Routes() []httpapi.Route {
	base := "/api/v1/t/{tenant_id}/orders"
	return httpapi.StrictRoutes(h, []httpapi.RouteSpec{
		{OperationID: "listOrders", Method: http.MethodGet, Pattern: base, Access: httpapi.AdminOrAPIKey, Scope: "orders:read", MaxBodySize: httpapi.DefaultMaxBodySize},
		{OperationID: "getOrder", Method: http.MethodGet, Pattern: base + "/{order_id}", Access: httpapi.AdminOrAPIKey, Scope: "orders:read", MaxBodySize: httpapi.DefaultMaxBodySize},
	})
}

func notFound() error { return apperrors.New(404, "not_found", "Order not found") }

func (h *HTTP) snapshot(ctx context.Context, requested uuid.UUID, fn func(*db.Queries) error) error {
	tenant, err := httpapi.AuthorizedTenant(ctx)
	if err != nil {
		return err
	}
	if tenant != requested.String() {
		return notFound()
	}
	if h.read == nil {
		return apperrors.New(503, "orders_unavailable", "Order persistence is unavailable")
	}
	err = h.read(ctx, tenant, func(q *db.Queries) error {
		// Re-check inside the transaction so an injected runner cannot widen scope.
		if current, e := httpapi.AuthorizedTenant(ctx); e != nil || current != tenant {
			return notFound()
		}
		return fn(q)
	})
	var domain *apperrors.Error
	switch {
	case err == nil, errors.As(err, &domain), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, pgx.ErrNoRows):
		return notFound()
	}
	return apperrors.New(503, "orders_unavailable", "Order persistence is unavailable")
}

func minor(v int64) string          { return strconv.FormatInt(v, 10) }
func pgID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func validState(s string) bool {
	switch contract.State(s) {
	case contract.PendingPayment, contract.Paid, contract.PartiallyFulfilled, contract.Fulfilled,
		contract.Cancelled, contract.PartiallyRefunded, contract.Refunded:
		return true
	}
	return false
}

// orderCursor binds the page position to the state filter it was issued for.
type orderCursor struct {
	Time  time.Time `json:"t"`
	ID    uuid.UUID `json:"id"`
	State string    `json:"s"`
}

func encodeOrderCursor(c orderCursor) string {
	c.Time = c.Time.UTC()
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// parseOrderCursor accepts only a cursor this server could have issued for the
// same filter: canonical base64url, exact fields, and byte-identical re-encoding.
func parseOrderCursor(encoded *string, state string) (orderCursor, bool, error) {
	if encoded == nil {
		return orderCursor{}, false, nil
	}
	bad := apperrors.New(400, "invalid_cursor", "Cursor is invalid")
	if len(*encoded) == 0 || len(*encoded) > 512 {
		return orderCursor{}, false, bad
	}
	raw, err := base64.RawURLEncoding.DecodeString(*encoded)
	if err != nil {
		return orderCursor{}, false, bad
	}
	var c orderCursor
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err != nil || decoder.More() || c.Time.IsZero() || c.ID == uuid.Nil ||
		c.State != state || encodeOrderCursor(c) != *encoded {
		return orderCursor{}, false, bad
	}
	return c, true, nil
}

func (h *HTTP) ListOrders(ctx context.Context, r httpapi.ListOrdersRequestObject) (httpapi.ListOrdersResponseObject, error) {
	limit := 25
	if r.Params.Limit != nil {
		limit = *r.Params.Limit
		if limit < 1 || limit > 100 {
			return nil, apperrors.New(400, "invalid_limit", "Limit must be between 1 and 100")
		}
	}
	state := pgtype.Text{}
	if r.Params.State != nil {
		if !validState(string(*r.Params.State)) {
			return nil, apperrors.New(400, "invalid_state", "Unknown order state")
		}
		state = pgtype.Text{String: string(*r.Params.State), Valid: true}
	}
	position, has, err := parseOrderCursor(r.Params.Cursor, state.String)
	if err != nil {
		return nil, err
	}
	page := httpapi.OrderPage{Items: []httpapi.OrderSummary{}}
	err = h.snapshot(ctx, r.TenantId, func(q *db.Queries) error {
		rows, e := q.ListAdminOrders(ctx, db.ListAdminOrdersParams{TenantID: pgID(r.TenantId), State: state, HasCursor: has,
			CursorCreatedAt: pgtype.Timestamptz{Time: position.Time, Valid: true}, CursorID: pgID(position.ID), PageSize: int32(limit + 1)})
		if e != nil {
			return e
		}
		more := len(rows) > limit
		if more {
			rows = rows[:limit]
		}
		for _, o := range rows {
			kind := httpapi.OrderSummaryCustomerKind("guest")
			if o.IsCustomer {
				kind = "customer"
			}
			page.Items = append(page.Items, httpapi.OrderSummary{Id: uuid.UUID(o.ID.Bytes), OrderNumber: o.OrderNumber,
				State: httpapi.OrderState(o.State), Currency: o.Currency, TotalMinor: minor(o.TotalMinor), CustomerKind: kind, CreatedAt: o.CreatedAt.Time.UTC()})
		}
		if more {
			last := rows[len(rows)-1]
			next := encodeOrderCursor(orderCursor{Time: last.CreatedAt.Time, ID: uuid.UUID(last.ID.Bytes), State: state.String})
			page.NextCursor = &next
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return httpapi.ListOrders200JSONResponse{Body: page, Headers: httpapi.ListOrders200ResponseHeaders{CacheControl: "no-store"}}, nil
}

func (h *HTTP) GetOrder(ctx context.Context, r httpapi.GetOrderRequestObject) (httpapi.GetOrderResponseObject, error) {
	var detail httpapi.OrderDetail
	err := h.snapshot(ctx, r.TenantId, func(q *db.Queries) error {
		tenant, id := pgID(r.TenantId), pgID(r.OrderId)
		o, e := q.GetAdminOrder(ctx, db.GetAdminOrderParams{TenantID: tenant, ID: id})
		if e != nil {
			return e
		}
		lines, e := q.ListAdminOrderLines(ctx, db.ListAdminOrderLinesParams{TenantID: tenant, OrderID: id})
		if e != nil {
			return e
		}
		adjustments, e := q.ListAdminOrderAdjustments(ctx, db.ListAdminOrderAdjustmentsParams{TenantID: tenant, OrderID: id})
		if e != nil {
			return e
		}
		method := nullable.NewNullNullable[string]()
		if o.ShippingMethod != "" {
			method = nullable.NewNullableWithValue(o.ShippingMethod)
		}
		detail = httpapi.OrderDetail{Id: uuid.UUID(o.ID.Bytes), OrderNumber: o.OrderNumber, State: httpapi.OrderState(o.State),
			Version: o.Version, Currency: o.Currency, Lines: make([]httpapi.OrderLine, 0, len(lines)),
			Adjustments:   make([]httpapi.OrderAdjustment, 0, len(adjustments)),
			Shipping:      httpapi.OrderShipping{AmountMinor: minor(o.ShippingMinor), Method: method},
			TaxTotalMinor: minor(o.TaxMinor), TotalMinor: minor(o.TotalMinor),
			CreatedAt: o.CreatedAt.Time.UTC(), UpdatedAt: o.UpdatedAt.Time.UTC()}
		for _, l := range lines {
			detail.Lines = append(detail.Lines, httpapi.OrderLine{LineId: uuid.UUID(l.ID.Bytes), SkuCode: l.Sku,
				Title: nullable.NewNullNullable[string](), Quantity: l.Quantity, UnitPriceMinor: minor(l.UnitPriceMinor),
				TaxMinor: minor(l.LineTaxMinor), LineTotalMinor: minor(l.LineTotalMinor)})
		}
		for _, a := range adjustments {
			detail.Adjustments = append(detail.Adjustments, httpapi.OrderAdjustment{Id: uuid.UUID(a.ID.Bytes), Reason: a.Reason, AmountMinor: minor(a.AmountMinor)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return httpapi.GetOrder200JSONResponse{Body: detail, Headers: httpapi.GetOrder200ResponseHeaders{
		ETag: `"` + strconv.FormatInt(detail.Version, 10) + `"`, CacheControl: "no-store"}}, nil
}
