// Catalog exposes a strict OpenAPI service and static scoped routes to API
// bootstrap. It uses platform-authenticated tenant context and sqlc only inside
// Config.Run; callers cannot authorize a tenant by passing its URL identifier.
package catalog

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type Config struct {
	Run func(context.Context, string, func(*db.Queries) error) error
}
type Service struct {
	httpapi.StrictServerInterface
	run func(context.Context, string, func(*db.Queries) error) error
}

func New(config Config) *Service { return &Service{run: config.Run} }
func authorize(ctx context.Context, requested uuid.UUID) (string, error) {
	tenant, err := httpapi.AuthorizedTenant(ctx)
	if err != nil {
		return "", err
	}
	if tenant != requested.String() {
		return "", apperrors.New(404, "not_found", "Catalog resource not found")
	}
	return tenant, nil
}
func (s *Service) transaction(ctx context.Context, requested uuid.UUID, fn func(*db.Queries) error) error {
	tenant, err := authorize(ctx, requested)
	if err != nil {
		return err
	}
	if s.run == nil {
		return apperrors.New(503, "catalog_unavailable", "Catalog persistence is unavailable")
	}
	// The runner receives only the stamped tenant. Check again before its DB
	// callback to make authorization invariant even with an injected runner.
	return mapError(s.run(ctx, tenant, func(q *db.Queries) error {
		current, err := httpapi.AuthorizedTenant(ctx)
		if err != nil || current != tenant {
			return apperrors.New(404, "not_found", "Catalog resource not found")
		}
		return fn(q)
	}))
}
func routeSpecs() []httpapi.RouteSpec {
	base := "/api/v1/t/{tenant_id}/products"
	product := base + "/{product_id}"
	variants := product + "/variants"
	variant := variants + "/{variant_id}"
	specs := []httpapi.RouteSpec{
		{OperationID: "createProduct", Method: http.MethodPost, Pattern: base, Scope: "catalog:write"},
		{OperationID: "listProducts", Method: http.MethodGet, Pattern: base, Scope: "catalog:read"},
		{OperationID: "getProduct", Method: http.MethodGet, Pattern: product, Scope: "catalog:read"},
		{OperationID: "updateProduct", Method: http.MethodPatch, Pattern: product, Scope: "catalog:write"},
		{OperationID: "archiveProduct", Method: http.MethodDelete, Pattern: product, Scope: "catalog:write"},
		{OperationID: "createVariant", Method: http.MethodPost, Pattern: variants, Scope: "catalog:write"},
		{OperationID: "listVariants", Method: http.MethodGet, Pattern: variants, Scope: "catalog:read"},
		{OperationID: "getVariant", Method: http.MethodGet, Pattern: variant, Scope: "catalog:read"},
		{OperationID: "updateVariant", Method: http.MethodPatch, Pattern: variant, Scope: "catalog:write"},
		{OperationID: "archiveVariant", Method: http.MethodDelete, Pattern: variant, Scope: "catalog:write"},
	}
	for i := range specs {
		specs[i].Access = httpapi.AdminOrAPIKey
		specs[i].MaxBodySize = httpapi.DefaultMaxBodySize
	}
	return specs
}
func (s *Service) Routes() []httpapi.Route { return httpapi.StrictRoutes(s, routeSpecs()) }
