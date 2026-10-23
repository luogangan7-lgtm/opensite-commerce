// Catalog storage uses generated sqlc over the authorized transaction supplied
// by bootstrap. Products, variants and variant option assignments belong here;
// platform HTTP handlers call this service, and no pool crosses this boundary.
package catalog

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func dbID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return apperrors.New(404, "not_found", "Catalog resource not found")
	}
	var pgerr *pgxadapter.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23503" {
		return invalid("Listing site or content revision does not belong to this product and locale")
	}
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		return apperrors.New(409, "catalog_conflict", "Product code, SKU or option combination already exists")
	}
	return err
}
func stale() error {
	return apperrors.New(412, "version_conflict", "Resource version does not match If-Match")
}
func archived() error {
	return apperrors.New(409, "resource_archived", "Archived resources cannot be changed")
}
func requireCurrent(version, want int64, status string) error {
	if version != want {
		return stale()
	}
	if status == "archived" {
		return archived()
	}
	return nil
}

func createVariant(ctx context.Context, q *db.Queries, tenant, product uuid.UUID, sku string, ids []uuid.UUID) (db.AppProductVariant, error) {
	parent, err := q.GetCatalogProductForUpdate(ctx, db.GetCatalogProductForUpdateParams{TenantID: dbID(tenant), ID: dbID(product)})
	if err != nil {
		return db.AppProductVariant{}, err
	}
	if parent.LifecycleStatus == "archived" {
		return db.AppProductVariant{}, archived()
	}
	if ids == nil {
		return db.AppProductVariant{}, invalid("option_value_ids is required")
	}
	seen := make(map[uuid.UUID]bool)
	values := make([]db.GetCatalogOptionValueRow, 0, len(ids))
	keys := make([]string, 0, len(ids))
	if len(ids) > 128 {
		return db.AppProductVariant{}, invalid("At most 128 option values are allowed")
	}
	for _, id := range ids {
		value, err := q.GetCatalogOptionValue(ctx, db.GetCatalogOptionValueParams{TenantID: dbID(tenant), ProductID: dbID(product), ID: dbID(id)})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.AppProductVariant{}, invalid("Option value must belong to the product")
		}
		if err != nil {
			return db.AppProductVariant{}, err
		}
		option := uuid.UUID(value.OptionID.Bytes)
		if seen[option] {
			return db.AppProductVariant{}, invalid("Only one value per option is allowed")
		}
		seen[option] = true
		values = append(values, value)
		keys = append(keys, option.String()+"="+uuid.UUID(value.ID.Bytes).String())
	}
	// Option assignments are immutable, so a variant must pick exactly one value
	// for every configured option at creation. Values already proved unique per
	// option and owned by this locked product, so equal counts mean full coverage.
	configured, err := q.CountCatalogProductOptions(ctx, db.CountCatalogProductOptionsParams{TenantID: dbID(tenant), ProductID: dbID(product)})
	if err != nil {
		return db.AppProductVariant{}, err
	}
	if int64(len(seen)) != configured {
		return db.AppProductVariant{}, invalid("A value is required for every product option")
	}
	sort.Strings(keys)
	variant, err := q.CreateVariant(ctx, db.CreateVariantParams{TenantID: dbID(tenant), ProductID: dbID(product), Sku: sku, OptionKey: strings.Join(keys, ";")})
	if err != nil {
		return variant, err
	}
	for _, value := range values {
		if err = q.AddVariantOptionValue(ctx, db.AddVariantOptionValueParams{TenantID: dbID(tenant), ProductID: dbID(product), VariantID: variant.ID, OptionID: value.OptionID, ValueID: value.ID}); err != nil {
			return db.AppProductVariant{}, err
		}
	}
	return variant, nil
}
