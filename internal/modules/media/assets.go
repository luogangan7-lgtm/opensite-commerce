// Asset link/delete handlers use the authorized sqlc transaction; products are
// read-only references here. Asset deletion rolls back link removal on release
// FK conflicts, and never removes a physical blob shared by other tenants.
package media

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func (s *Service) LinkProductMedia(ctx context.Context, r httpapi.LinkProductMediaRequestObject) (httpapi.LinkProductMediaResponseObject, error) {
	var result httpapi.ProductMedia
	err := s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		if r.Body == nil {
			return invalid("Media link is required")
		}
		b := r.Body
		switch string(b.Role) {
		case "main", "gallery", "detail", "video", "size_chart":
		default:
			return invalid("Unsupported media role")
		}
		order := int32(0)
		if b.SortOrder != nil {
			order = int32(*b.SortOrder)
		}
		if order < 0 {
			return invalid("sort_order must be nonnegative")
		}
		// ponytail: plain read, not FOR SHARE (media role has no products UPDATE
		// grant); an archive committing concurrently can still race one link in.
		status, e := q.MediaProductStatus(ctx, db.MediaProductStatusParams{TenantID: dbID(r.TenantId), ID: dbID(r.ProductId)})
		if e != nil {
			return e
		}
		if status == "archived" {
			return apperrors.New(409, "resource_archived", "Archived products cannot be changed")
		}
		if _, e = q.LockMediaAsset(ctx, db.LockMediaAssetParams{TenantID: dbID(r.TenantId), ID: dbID(b.MediaId)}); e != nil {
			return e
		}
		e = q.LinkProductMedia(ctx, db.LinkProductMediaParams{TenantID: dbID(r.TenantId), ProductID: dbID(r.ProductId), MediaID: dbID(b.MediaId), Role: string(b.Role), SortOrder: order})
		if e != nil {
			return e
		}
		result = httpapi.ProductMedia{ProductId: r.ProductId, MediaId: b.MediaId, Role: httpapi.ProductMediaRole(b.Role), SortOrder: order}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return httpapi.LinkProductMedia201JSONResponse(result), nil
}
func (s *Service) DeleteMedia(ctx context.Context, r httpapi.DeleteMediaRequestObject) (httpapi.DeleteMediaResponseObject, error) {
	err := s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		if _, e := q.LockMediaAsset(ctx, db.LockMediaAssetParams{TenantID: dbID(r.TenantId), ID: dbID(r.MediaId)}); e != nil {
			return e
		}
		if e := q.DeleteProductMediaLinks(ctx, db.DeleteProductMediaLinksParams{TenantID: dbID(r.TenantId), MediaID: dbID(r.MediaId)}); e != nil {
			return e
		}
		return q.DeleteMediaAsset(ctx, db.DeleteMediaAssetParams{TenantID: dbID(r.TenantId), ID: dbID(r.MediaId)})
	})
	if err != nil {
		return nil, err
	}
	return httpapi.DeleteMedia204Response{}, nil
}
