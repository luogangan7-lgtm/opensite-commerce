// Media owns upload/blob/asset metadata and product links. Bootstrap calls its
// strict routes with a dedicated media-role tenant transaction and LocalFS.
// Filenames never become keys; READY requires verified durable content; physical
// blobs are shared across tenants and may only be reclaimed by a future GC.
package media

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/storage/local"
)

type Config struct {
	Run  func(context.Context, string, func(*db.Queries) error) error
	Root string
}
type Service struct {
	generated.StrictServerInterface
	run     func(context.Context, string, func(*db.Queries) error) error
	store   *local.Store
	initErr error
}

func New(c Config) *Service {
	if c.Root == "" {
		c.Root = os.Getenv("OPENSITE_MEDIA_ROOT")
	}
	s := &Service{run: c.Run}
	if c.Root == "" {
		s.initErr = errors.New("media root is required")
	} else {
		s.store, s.initErr = local.New(c.Root, MaxBytes)
	}
	return s
}
func (s *Service) transaction(ctx context.Context, id uuid.UUID, fn func(*db.Queries) error) error {
	tenant, err := httpapi.AuthorizedTenant(ctx)
	if err != nil {
		return err
	}
	if tenant != id.String() {
		return notFound()
	}
	if s.run == nil || s.initErr != nil {
		return apperrors.New(503, "media_unavailable", "Media persistence is unavailable")
	}
	err = s.run(ctx, tenant, func(q *db.Queries) error {
		current, e := httpapi.AuthorizedTenant(ctx)
		if e != nil || current != tenant {
			return notFound()
		}
		return fn(q)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound()
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23503", "23505", "55000":
			return apperrors.New(409, "media_conflict", "Media is referenced or conflicts with existing data")
		}
	}
	return err
}
func dbID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
func notFound() error               { return apperrors.New(404, "not_found", "Media resource not found") }
func invalid(detail string) error   { return apperrors.New(422, "invalid_media", detail) }
func conflict(detail string) error  { return apperrors.New(409, "upload_conflict", detail) }
func (s *Service) Routes() []httpapi.Route {
	base := "/api/v1/t/{tenant_id}"
	specs := []httpapi.RouteSpec{
		{OperationID: "createMediaUpload", Method: http.MethodPost, Pattern: base + "/media/uploads", Scope: "media:write"},
		{OperationID: "putMediaUploadContent", Method: http.MethodPut, Pattern: base + "/media/uploads/{upload_id}/content", Scope: "media:write", MaxBodySize: MaxBytes},
		{OperationID: "completeMediaUpload", Method: http.MethodPost, Pattern: base + "/media/uploads/{upload_id}/complete", Scope: "media:write"},
		{OperationID: "linkProductMedia", Method: http.MethodPost, Pattern: base + "/products/{product_id}/media", Scope: "media:write"},
		{OperationID: "deleteMedia", Method: http.MethodDelete, Pattern: base + "/media/{media_id}", Scope: "media:write"},
	}
	for i := range specs {
		specs[i].Access = httpapi.AdminOrAPIKey
		if specs[i].MaxBodySize == 0 {
			specs[i].MaxBodySize = httpapi.DefaultMaxBodySize
		}
	}
	return httpapi.StrictRoutes(s, specs)
}
