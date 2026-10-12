// Responsibilities: implement only catalog-owned strict generated operations.
// Dependencies: sqlc catalog queries, generated DTOs, platform authorization/errors;
// caller: API bootstrap. Invariants: tenant-stamped transactions, immutable option
// assignments, soft archive and parent-row locks serialize product/variant writes.
package catalog

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
)

func productDTO(p db.AppProduct) generated.Product {
	return generated.Product{Id: uuid.UUID(p.ID.Bytes), ProductCode: p.ProductCode, LifecycleStatus: generated.ProductLifecycleStatus(p.LifecycleStatus), CreatedAt: p.CreatedAt.Time, Version: p.Version}
}
func variantDTO(v db.AppProductVariant, values []uuid.UUID) generated.Variant {
	if values == nil {
		values = []uuid.UUID{}
	}
	return generated.Variant{Id: uuid.UUID(v.ID.Bytes), ProductId: uuid.UUID(v.ProductID.Bytes), Sku: v.Sku, LifecycleStatus: generated.VariantLifecycleStatus(v.LifecycleStatus), CreatedAt: v.CreatedAt.Time, Version: v.Version, OptionValueIds: values}
}
func optionValues(ctx context.Context, q *db.Queries, tenant, product uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
	rows, err := q.ListCatalogVariantOptionValues(ctx, db.ListCatalogVariantOptionValuesParams{TenantID: dbID(tenant), ProductID: dbID(product)})
	if err != nil {
		return nil, err
	}
	result := make(map[uuid.UUID][]uuid.UUID)
	for _, row := range rows {
		id := uuid.UUID(row.VariantID.Bytes)
		result[id] = append(result[id], uuid.UUID(row.ValueID.Bytes))
	}
	return result, nil
}
func optionalCode(value *string) (pgtype.Text, error) {
	if value == nil {
		return pgtype.Text{}, nil
	}
	code, err := normalizeCode(*value)
	return pgtype.Text{String: code, Valid: err == nil}, err
}
func optionalLifecycle(value *string) (pgtype.Text, error) {
	if value == nil {
		return pgtype.Text{}, nil
	}
	err := lifecycle(*value)
	return pgtype.Text{String: *value, Valid: err == nil}, err
}

func (s *Service) CreateProduct(ctx context.Context, r generated.CreateProductRequestObject) (generated.CreateProductResponseObject, error) {
	if _, err := authorize(ctx, r.TenantId); err != nil {
		return nil, err
	}
	if r.Body == nil {
		return nil, invalid("Product body is required")
	}
	code, err := normalizeCode(r.Body.ProductCode)
	if err != nil {
		return nil, err
	}
	var result db.AppProduct
	err = s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		var e error
		result, e = q.CreateProduct(ctx, db.CreateProductParams{TenantID: dbID(r.TenantId), ProductCode: code})
		return e
	})
	if err != nil {
		return nil, err
	}
	return generated.CreateProduct201JSONResponse(productDTO(result)), nil
}
func (s *Service) GetProduct(ctx context.Context, r generated.GetProductRequestObject) (generated.GetProductResponseObject, error) {
	var result db.AppProduct
	err := s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		var e error
		result, e = q.GetProduct(ctx, db.GetProductParams{TenantID: dbID(r.TenantId), ID: dbID(r.ProductId)})
		return e
	})
	if err != nil {
		return nil, err
	}
	return generated.GetProduct200JSONResponse(productDTO(result)), nil
}
func (s *Service) ListProducts(ctx context.Context, r generated.ListProductsRequestObject) (generated.ListProductsResponseObject, error) {
	if _, err := authorize(ctx, r.TenantId); err != nil {
		return nil, err
	}
	limit, err := pageLimit(r.Params.PageSize)
	if err != nil {
		return nil, err
	}
	at, id, has, err := parseCursor(r.Params.Cursor)
	if err != nil {
		return nil, err
	}
	result := generated.ProductPage{Items: []generated.Product{}}
	err = s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		rows, e := q.ListProducts(ctx, db.ListProductsParams{TenantID: dbID(r.TenantId), HasCursor: has, CursorCreatedAt: pgtype.Timestamptz{Time: at, Valid: true}, CursorID: dbID(id), PageSize: limit + 1})
		if e != nil {
			return e
		}
		more := len(rows) > int(limit)
		if more {
			rows = rows[:limit]
		}
		for _, row := range rows {
			result.Items = append(result.Items, productDTO(row))
		}
		if more {
			last := rows[len(rows)-1]
			c := encodeCursor(last.CreatedAt.Time, uuid.UUID(last.ID.Bytes))
			result.NextCursor = &c
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return generated.ListProducts200JSONResponse(result), nil
}
func (s *Service) UpdateProduct(ctx context.Context, r generated.UpdateProductRequestObject) (generated.UpdateProductResponseObject, error) {
	if _, err := authorize(ctx, r.TenantId); err != nil {
		return nil, err
	}
	version, err := parseVersion(r.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	if r.Body == nil {
		return nil, invalid("Product body is required")
	}
	if r.Body.ProductCode == nil && r.Body.LifecycleStatus == nil {
		return nil, invalid("At least one product change is required")
	}
	code, err := optionalCode(r.Body.ProductCode)
	if err != nil {
		return nil, err
	}
	var status *string
	if r.Body.LifecycleStatus != nil {
		v := string(*r.Body.LifecycleStatus)
		status = &v
	}
	state, err := optionalLifecycle(status)
	if err != nil {
		return nil, err
	}
	var result db.AppProduct
	err = s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		current, e := q.GetCatalogProductForUpdate(ctx, db.GetCatalogProductForUpdateParams{TenantID: dbID(r.TenantId), ID: dbID(r.ProductId)})
		if e != nil {
			return e
		}
		if e = requireCurrent(current.Version, version, current.LifecycleStatus); e != nil {
			return e
		}
		result, e = q.UpdateProduct(ctx, db.UpdateProductParams{TenantID: dbID(r.TenantId), ID: dbID(r.ProductId), Version: version, ProductCode: code, LifecycleStatus: state})
		return e
	})
	if err != nil {
		return nil, err
	}
	return generated.UpdateProduct200JSONResponse(productDTO(result)), nil
}
func (s *Service) ArchiveProduct(ctx context.Context, r generated.ArchiveProductRequestObject) (generated.ArchiveProductResponseObject, error) {
	if _, err := authorize(ctx, r.TenantId); err != nil {
		return nil, err
	}
	version, err := parseVersion(r.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	err = s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		current, e := q.GetCatalogProductForUpdate(ctx, db.GetCatalogProductForUpdateParams{TenantID: dbID(r.TenantId), ID: dbID(r.ProductId)})
		if e != nil {
			return e
		}
		if e = requireCurrent(current.Version, version, current.LifecycleStatus); e != nil {
			return e
		}
		_, e = q.ArchiveProduct(ctx, db.ArchiveProductParams{TenantID: dbID(r.TenantId), ID: dbID(r.ProductId), Version: version})
		return e
	})
	if err != nil {
		return nil, err
	}
	return generated.ArchiveProduct204Response{}, nil
}
func (s *Service) CreateVariant(ctx context.Context, r generated.CreateVariantRequestObject) (generated.CreateVariantResponseObject, error) {
	if _, err := authorize(ctx, r.TenantId); err != nil {
		return nil, err
	}
	if r.Body == nil {
		return nil, invalid("Variant body is required")
	}
	sku, err := normalizeCode(r.Body.Sku)
	if err != nil {
		return nil, err
	}
	var result generated.Variant
	err = s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		v, e := createVariant(ctx, q, r.TenantId, r.ProductId, sku, r.Body.OptionValueIds)
		if e != nil {
			return e
		}
		values, e := optionValues(ctx, q, r.TenantId, r.ProductId)
		if e != nil {
			return e
		}
		result = variantDTO(v, values[uuid.UUID(v.ID.Bytes)])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return generated.CreateVariant201JSONResponse(result), nil
}
func (s *Service) GetVariant(ctx context.Context, r generated.GetVariantRequestObject) (generated.GetVariantResponseObject, error) {
	var result generated.Variant
	err := s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		v, e := q.GetVariant(ctx, db.GetVariantParams{TenantID: dbID(r.TenantId), ProductID: dbID(r.ProductId), ID: dbID(r.VariantId)})
		if e != nil {
			return e
		}
		values, e := optionValues(ctx, q, r.TenantId, r.ProductId)
		if e != nil {
			return e
		}
		result = variantDTO(v, values[r.VariantId])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return generated.GetVariant200JSONResponse(result), nil
}
func (s *Service) ListVariants(ctx context.Context, r generated.ListVariantsRequestObject) (generated.ListVariantsResponseObject, error) {
	result := generated.VariantList{Items: []generated.Variant{}}
	err := s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		if _, e := q.GetProduct(ctx, db.GetProductParams{TenantID: dbID(r.TenantId), ID: dbID(r.ProductId)}); e != nil {
			return e
		}
		rows, e := q.ListVariants(ctx, db.ListVariantsParams{TenantID: dbID(r.TenantId), ProductID: dbID(r.ProductId)})
		if e != nil {
			return e
		}
		values, e := optionValues(ctx, q, r.TenantId, r.ProductId)
		if e != nil {
			return e
		}
		for _, v := range rows {
			result.Items = append(result.Items, variantDTO(v, values[uuid.UUID(v.ID.Bytes)]))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return generated.ListVariants200JSONResponse(result), nil
}
func writableVariant(ctx context.Context, q *db.Queries, tenant, product, variant uuid.UUID, version int64) error {
	p, err := q.GetCatalogProductForUpdate(ctx, db.GetCatalogProductForUpdateParams{TenantID: dbID(tenant), ID: dbID(product)})
	if err != nil {
		return err
	}
	v, err := q.GetVariant(ctx, db.GetVariantParams{TenantID: dbID(tenant), ProductID: dbID(product), ID: dbID(variant)})
	if err != nil {
		return err
	}
	// Resolve the variant before checking the parent's state so a foreign ID
	// remains 404 even when the requested parent is archived.
	if p.LifecycleStatus == "archived" {
		return archived()
	}
	return requireCurrent(v.Version, version, v.LifecycleStatus)
}
func (s *Service) UpdateVariant(ctx context.Context, r generated.UpdateVariantRequestObject) (generated.UpdateVariantResponseObject, error) {
	if _, err := authorize(ctx, r.TenantId); err != nil {
		return nil, err
	}
	version, err := parseVersion(r.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	if r.Body == nil {
		return nil, invalid("Variant body is required")
	}
	if r.Body.Sku == nil && r.Body.LifecycleStatus == nil {
		return nil, invalid("At least one variant change is required")
	}
	sku, err := optionalCode(r.Body.Sku)
	if err != nil {
		return nil, err
	}
	var status *string
	if r.Body.LifecycleStatus != nil {
		v := string(*r.Body.LifecycleStatus)
		status = &v
	}
	state, err := optionalLifecycle(status)
	if err != nil {
		return nil, err
	}
	var result generated.Variant
	err = s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		if e := writableVariant(ctx, q, r.TenantId, r.ProductId, r.VariantId, version); e != nil {
			return e
		}
		v, e := q.UpdateVariant(ctx, db.UpdateVariantParams{TenantID: dbID(r.TenantId), ProductID: dbID(r.ProductId), ID: dbID(r.VariantId), Version: version, Sku: sku, LifecycleStatus: state})
		if e != nil {
			return e
		}
		values, e := optionValues(ctx, q, r.TenantId, r.ProductId)
		if e != nil {
			return e
		}
		result = variantDTO(v, values[r.VariantId])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return generated.UpdateVariant200JSONResponse(result), nil
}
func (s *Service) ArchiveVariant(ctx context.Context, r generated.ArchiveVariantRequestObject) (generated.ArchiveVariantResponseObject, error) {
	if _, err := authorize(ctx, r.TenantId); err != nil {
		return nil, err
	}
	version, err := parseVersion(r.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	err = s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		if e := writableVariant(ctx, q, r.TenantId, r.ProductId, r.VariantId, version); e != nil {
			return e
		}
		_, e := q.ArchiveVariant(ctx, db.ArchiveVariantParams{TenantID: dbID(r.TenantId), ProductID: dbID(r.ProductId), ID: dbID(r.VariantId), Version: version})
		return e
	})
	if err != nil {
		return nil, err
	}
	return generated.ArchiveVariant204Response{}, nil
}
