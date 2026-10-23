// Responsibility: staff locale content/listing operations and shared CSV allocation.
// Dependencies: sqlc catalog queries and HTTP port; callers: bootstrap and CSV.
// Invariants: one product lock before latest-read/CAS/append, DB timestamps,
// tenant-stamped transactions, no published pointer change on content append.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func contentETag(revision int64) *string { value := fmt.Sprintf(`"%d"`, revision); return &value }

// lockContent is shared by CSV and staff commands, including listing writes.
func lockContent(ctx context.Context, q *db.Queries, tenant, product pgtype.UUID) error {
	status, err := q.LockCatalogContent(ctx, db.LockCatalogContentParams{TenantID: tenant, ID: product})
	if err != nil {
		return err
	}
	if status == "archived" {
		return archived()
	}
	return nil
}
func appendContent(ctx context.Context, q *db.Queries, p db.AppendCatalogContentParams) error {
	if err := lockContent(ctx, q, p.TenantID, p.ProductID); err != nil {
		return err
	}
	return q.AppendCatalogContent(ctx, p)
}
func contentVersion(raw string) (int64, error) {
	if raw == `"0"` {
		return 0, nil
	}
	return parseVersion(raw)
}
func readContent(ctx context.Context, q *db.Queries, tenant, product uuid.UUID, locale string) (out httpapi.StaffProductContent, err error) {
	b, err := q.ReadCatalogContent(ctx, db.ReadCatalogContentParams{TenantID: dbID(tenant), ProductID: dbID(product), Locale: locale})
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	return out, err
}
func checkContent(ctx context.Context, q *db.Queries, tenant, product uuid.UUID, locale, raw string) error {
	v, err := contentVersion(raw)
	if err != nil {
		return err
	}
	if err = lockContent(ctx, q, dbID(tenant), dbID(product)); err != nil {
		return err
	}
	current, err := readContent(ctx, q, tenant, product, locale)
	if err == nil && current.Revision != v {
		return stale()
	}
	return err
}
func (s *Service) GetProductContent(ctx context.Context, r httpapi.GetProductContentRequestObject) (httpapi.GetProductContentResponseObject, error) {
	var result httpapi.StaffProductContent
	err := s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		var err error
		result, err = readContent(ctx, q, r.TenantId, r.ProductId, r.Locale)
		return err
	})
	return httpapi.GetProductContent200JSONResponse{Body: result, Headers: httpapi.GetProductContent200ResponseHeaders{ETag: contentETag(result.Revision)}}, err
}
func (s *Service) SetProductContent(ctx context.Context, r httpapi.SetProductContentRequestObject) (httpapi.SetProductContentResponseObject, error) {
	if _, err := authorize(ctx, r.TenantId); err != nil {
		return nil, err
	}
	if _, err := contentVersion(r.Params.IfMatch); err != nil {
		return nil, err
	}
	if r.Body == nil || strings.TrimSpace(r.Body.Title) == "" || len(r.Body.Title) > 4096 {
		return nil, invalid("Invalid product title")
	}
	body, _ := json.Marshal(r.Body.BodyBlocks)
	seo, _ := json.Marshal(r.Body.Seo)
	if len(body) > 65536 || len(seo) > 16384 {
		return nil, invalid("Product content exceeds limits")
	}
	var result httpapi.StaffProductContent
	err := s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		if err := checkContent(ctx, q, r.TenantId, r.ProductId, r.Locale, r.Params.IfMatch); err != nil {
			return err
		}
		if err := appendContent(ctx, q, db.AppendCatalogContentParams{TenantID: dbID(r.TenantId), ProductID: dbID(r.ProductId), Locale: r.Locale, Title: r.Body.Title, BodyBlocks: body, Seo: seo}); err != nil {
			return err
		}
		var err error
		result, err = readContent(ctx, q, r.TenantId, r.ProductId, r.Locale)
		return err
	})
	return httpapi.SetProductContent200JSONResponse{Body: result, Headers: httpapi.SetProductContent200ResponseHeaders{ETag: contentETag(result.Revision)}}, err
}
func listing(ctx context.Context, q *db.Queries, tenant, product, site uuid.UUID, locale string) (out httpapi.StaffProductListing, err error) {
	b, err := q.ReadCatalogListing(ctx, db.ReadCatalogListingParams{TenantID: dbID(tenant), ProductID: dbID(product), SiteID: dbID(site), Locale: locale})
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	return out, err
}
func (s *Service) GetProductListing(ctx context.Context, r httpapi.GetProductListingRequestObject) (httpapi.GetProductListingResponseObject, error) {
	var result httpapi.StaffProductListing
	err := s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		var err error
		result, err = listing(ctx, q, r.TenantId, r.ProductId, r.SiteId, r.Locale)
		return err
	})
	return httpapi.GetProductListing200JSONResponse{Body: result, Headers: httpapi.GetProductListing200ResponseHeaders{ETag: contentETag(result.Revision)}}, err
}
func (s *Service) SetProductListing(ctx context.Context, r httpapi.SetProductListingRequestObject) (httpapi.SetProductListingResponseObject, error) {
	if _, err := authorize(ctx, r.TenantId); err != nil {
		return nil, err
	}
	if _, err := contentVersion(r.Params.IfMatch); err != nil {
		return nil, err
	}
	if r.Body == nil {
		return nil, invalid("Listing body is required")
	}
	var result httpapi.StaffProductListing
	err := s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		if err := checkContent(ctx, q, r.TenantId, r.ProductId, r.Locale, r.Params.IfMatch); err != nil {
			return err
		}
		if _, err := listing(ctx, q, r.TenantId, r.ProductId, r.SiteId, r.Locale); err != nil {
			return err
		}
		if err := q.WriteCatalogListing(ctx, db.WriteCatalogListingParams{TenantID: dbID(r.TenantId), ProductID: dbID(r.ProductId), SiteID: dbID(r.SiteId), Locale: r.Locale, Slug: r.Body.Slug, Listed: r.Body.Listed, ContentRevision: r.Body.ContentRevision}); err != nil {
			return err
		}
		var err error
		result, err = listing(ctx, q, r.TenantId, r.ProductId, r.SiteId, r.Locale)
		return err
	})
	return httpapi.SetProductListing200JSONResponse{Body: result, Headers: httpapi.SetProductListing200ResponseHeaders{ETag: contentETag(result.Revision)}}, err
}
