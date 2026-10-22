// Package pricing owns staff price-list/tier commands and scoped projections.
// Dependencies: sqlc, money and platform HTTP; caller: bootstrap. The DB pricing
// capability owns writes, tenant currency locks and list-wide version fencing.
package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type Config struct {
	Run func(context.Context, string, func(*db.Queries) error) error
}
type Service struct {
	httpapi.StrictServerInterface
	run func(context.Context, string, func(*db.Queries) error) error
}

func New(c Config) *Service {
	return &Service{run: c.Run}
}
func id(v uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: v, Valid: true}
}
func invalid() error {
	return apperrors.New(422, "invalid_pricing", "Invalid price list or tier")
}
func version(raw string) (int64, error) {
	if raw == "" {
		return 0, apperrors.New(428, "precondition_required", "If-Match is required")
	}
	if len(raw) >= 3 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		n, err := strconv.ParseInt(raw[1:len(raw)-1], 10, 64)
		if err == nil && n > 0 && strconv.Quote(strconv.FormatInt(n, 10)) == raw {
			return n, nil
		}
	}
	return 0, apperrors.New(400, "invalid_if_match", "If-Match must be a quoted positive decimal version")
}
func (s *Service) query(ctx context.Context, tenant uuid.UUID, fn func(*db.Queries) ([]byte, error)) ([]byte, error) {
	t, err := httpapi.AuthorizedTenant(ctx)
	if err != nil {
		return nil, err
	}
	if t != tenant.String() {
		return nil, apperrors.New(404, "not_found", "Price resource not found")
	}
	if s.run == nil {
		return nil, apperrors.New(503, "pricing_unavailable", "Pricing persistence unavailable")
	}
	var result []byte
	err = s.run(ctx, t, func(q *db.Queries) error {
		var e error
		result, e = fn(q)
		return e
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = apperrors.New(404, "not_found", "Price resource not found")
	}
	var pe *pgxadapter.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "P0002":
			err = apperrors.New(404, "not_found", "Price resource not found")
		case "P4120", "40001":
			err = apperrors.New(412, "version_conflict", "Price list version changed")
		case "P4090":
			err = apperrors.New(409, "settlement_currency_required", "Configure tenant settlement currency first")
		case "P4091":
			err = apperrors.New(409, "currency_mismatch", "Price list currency must equal settlement currency")
		case "P4092":
			err = apperrors.New(409, "resource_archived", "Archived price lists cannot be changed")
		case "23514", "23503", "23502", "22003", "22007", "22008", "22P02":
			err = invalid()
		}
	}
	return result, err
}
func decode[T any](b []byte, err error) (result T, e error) {
	if err != nil {
		return result, err
	}
	return result, json.Unmarshal(b, &result)
}
func (s *Service) writeList(ctx context.Context, tenant uuid.UUID, key pgtype.UUID, v int64, body *httpapi.StaffPriceListInput) (httpapi.StaffPriceList, error) {
	if body == nil || (money.Money{Currency: body.Currency}).Validate() != nil {
		return httpapi.StaffPriceList{}, invalid()
	}
	b, err := json.Marshal(body)
	if err != nil {
		return httpapi.StaffPriceList{}, err
	}
	return decode[httpapi.StaffPriceList](s.query(ctx, tenant, func(q *db.Queries) ([]byte, error) {
		return q.WriteStaffPriceList(ctx, db.WriteStaffPriceListParams{ID: key, Version: v, Body: b})
	}))
}
func (s *Service) CreatePriceList(ctx context.Context, r httpapi.CreatePriceListRequestObject) (httpapi.CreatePriceListResponseObject, error) {
	p, err := s.writeList(ctx, r.TenantId, pgtype.UUID{}, 0, r.Body)
	return httpapi.CreatePriceList201JSONResponse(p), err
}
func (s *Service) UpdatePriceList(ctx context.Context, r httpapi.UpdatePriceListRequestObject) (httpapi.UpdatePriceListResponseObject, error) {
	v, err := version(r.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	p, err := s.writeList(ctx, r.TenantId, id(r.PriceListId), v, r.Body)
	return httpapi.UpdatePriceList200JSONResponse(p), err
}
func (s *Service) GetPriceList(ctx context.Context, r httpapi.GetPriceListRequestObject) (httpapi.GetPriceListResponseObject, error) {
	p, err := decode[httpapi.StaffPriceList](s.query(ctx, r.TenantId, func(q *db.Queries) ([]byte, error) {
		return q.GetStaffPriceList(ctx, db.GetStaffPriceListParams{TenantID: id(r.TenantId), ID: id(r.PriceListId)})
	}))
	return httpapi.GetPriceList200JSONResponse(p), err
}
func (s *Service) ListPriceLists(ctx context.Context, r httpapi.ListPriceListsRequestObject) (httpapi.ListPriceListsResponseObject, error) {
	after := uuid.Nil
	if r.Params.After != nil {
		after = *r.Params.After
	}
	items, err := decode[[]httpapi.StaffPriceList](s.query(ctx, r.TenantId, func(q *db.Queries) ([]byte, error) {
		return q.ListStaffPriceLists(ctx, db.ListStaffPriceListsParams{TenantID: id(r.TenantId), AfterID: id(after)})
	}))
	page := httpapi.StaffPriceListPage{Items: items}
	if len(items) > 100 {
		page.Items = items[:100]
		page.NextCursor = &items[99].Id
	}
	return httpapi.ListPriceLists200JSONResponse(page), err
}
func (s *Service) entry(ctx context.Context, tenant, list, variant uuid.UUID, min int64, amount pgtype.Int8, raw string) (httpapi.StaffPriceList, error) {
	v, err := version(raw)
	if err != nil {
		return httpapi.StaffPriceList{}, err
	}
	return decode[httpapi.StaffPriceList](s.query(ctx, tenant, func(q *db.Queries) ([]byte, error) {
		return q.WriteStaffPriceEntry(ctx, db.WriteStaffPriceEntryParams{ListID: id(list), SkuID: id(variant), MinQuantity: min, Amount: amount, Version: v})
	}))
}
func (s *Service) SetVariantPrice(ctx context.Context, r httpapi.SetVariantPriceRequestObject) (httpapi.SetVariantPriceResponseObject, error) {
	if r.Body == nil || r.Body.UnitAmount < 0 || r.MinQuantity < 1 {
		return nil, invalid()
	}
	p, err := s.entry(ctx, r.TenantId, r.PriceListId, r.VariantId, r.MinQuantity, pgtype.Int8{Int64: r.Body.UnitAmount, Valid: true}, r.Params.IfMatch)
	return httpapi.SetVariantPrice200JSONResponse(p), err
}
func (s *Service) ClearVariantPrice(ctx context.Context, r httpapi.ClearVariantPriceRequestObject) (httpapi.ClearVariantPriceResponseObject, error) {
	p, err := s.entry(ctx, r.TenantId, r.PriceListId, r.VariantId, r.MinQuantity, pgtype.Int8{}, r.Params.IfMatch)
	return httpapi.ClearVariantPrice200JSONResponse(p), err
}
func (s *Service) Routes() []httpapi.Route {
	base := "/api/v1/t/{tenant_id}/price-lists"
	list := base + "/{price_list_id}"
	prices := list + "/variants/{variant_id}/prices"
	var specs []httpapi.RouteSpec
	for _, r := range []struct{ method, path, operation string }{
		{"POST", base, "createPriceList"}, {"GET", base, "listPriceLists"}, {"GET", list, "getPriceList"},
		{"PUT", list, "updatePriceList"},
		{"PUT", prices + "/{min_quantity}", "setVariantPrice"}, {"DELETE", prices + "/{min_quantity}", "clearVariantPrice"},
	} {
		specs = append(specs, httpapi.RouteSpec{Method: r.method, Pattern: r.path, OperationID: r.operation, Access: httpapi.AdminOrAPIKey, Scope: "catalog:write", MaxBodySize: httpapi.DefaultMaxBodySize})
	}
	return httpapi.StrictRoutes(s, specs)
}
