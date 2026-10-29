// Upload handlers use sqlc and the private LocalFS, called only through the
// authenticated strict service. Reserve uses shared C-04 claims and exact
// durable replay; upload row locks serialize content/completion. Actual stream
// bytes, size, hash and image structure determine readiness.
package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"github.com/oapi-codegen/nullable"
)

func session(u db.AppMediaUpload) httpapi.UploadSession {
	out := httpapi.UploadSession{Id: uuid.UUID(u.ID.Bytes), Status: httpapi.UploadSessionStatus(u.Status), ExpiresAt: u.ExpiresAt.Time}
	if u.MediaID.Valid {
		out.MediaId = nullable.NewNullableWithValue(uuid.UUID(u.MediaID.Bytes))
	} else {
		out.MediaId = nullable.NewNullNullable[uuid.UUID]()
	}
	return out
}
func declared(u db.AppMediaUpload) string { return u.DeclaredMime.String }
func (s *Service) CreateMediaUpload(ctx context.Context, r httpapi.CreateMediaUploadRequestObject) (httpapi.CreateMediaUploadResponseObject, error) {
	tenant, err := httpapi.AuthorizedTenant(ctx)
	if err != nil {
		return nil, err
	}
	if tenant != r.TenantId.String() {
		return nil, notFound()
	}
	actor, err := httpapi.AuthorizedActor(ctx)
	if err != nil {
		return nil, err
	}
	if s.initErr != nil {
		return nil, apperrors.New(503, "media_unavailable", "Media persistence is unavailable")
	}
	if r.Body == nil {
		return nil, invalid("Upload metadata is required")
	}
	b := r.Body
	if !validFilename(b.Filename) {
		return nil, invalid("Filename must not contain path separators or control characters")
	}
	if int64(b.SizeBytes) > MaxBytes {
		return nil, apperrors.New(413, "media_too_large", "Upload exceeds 10 MiB")
	}
	if b.SizeBytes < 1 {
		return nil, invalid("size_bytes must be positive")
	}
	hash, err := hex.DecodeString(b.Sha256)
	if err != nil || len(hash) != 32 {
		return nil, invalid("sha256 must be 64 hexadecimal characters")
	}
	mime := pgtype.Text{}
	if b.DeclaredMime != nil {
		if !allowedMIME(*b.DeclaredMime) {
			return nil, apperrors.New(415, "invalid_media", "Unsupported image MIME")
		}
		mime = pgtype.Text{String: *b.DeclaredMime, Valid: true}
	}
	command, replay, err := httpapi.BeginCommand(ctx, s.reserve, "createMediaUpload")
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return reserveResponse{*replay}, nil
	}
	// The claim is admitted first; the upload and exact response complete in
	// one authorized transaction. Store/fencing/commit errors roll both back.
	err = s.reserve.Run(ctx, tenant, func(txctx context.Context, tx pgx.Tx) error {
		if current, e := httpapi.AuthorizedTenant(txctx); e != nil || current != tenant {
			return notFound()
		}
		if current, e := httpapi.AuthorizedActor(txctx); e != nil || current != actor {
			return notFound()
		}
		result, err := db.New(tx).ReserveMediaUpload(txctx, db.ReserveMediaUploadParams{TenantID: dbID(r.TenantId), OriginalFilename: b.Filename, DeclaredMime: mime, ExpectedSize: pgtype.Int8{Int64: int64(b.SizeBytes), Valid: true}, ExpectedSha256: hash})
		if err != nil {
			return err
		}
		return command.Complete(txctx, tx, http.StatusCreated, session(result))
	})
	if err != nil {
		return nil, err
	}
	return reserveResponse{command.Response()}, nil
}

type reserveResponse struct{ idempotency.Response }

func (r reserveResponse) VisitCreateMediaUploadResponse(w http.ResponseWriter) error {
	return httpapi.WriteCommandResponse(w, r.Response)
}

func readContent(ctx context.Context, r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, invalid("Content is required")
	}
	b, err := io.ReadAll(io.LimitReader(contextReader{ctx, r}, MaxBytes+1))
	var over *http.MaxBytesError
	if int64(len(b)) > MaxBytes || errors.As(err, &over) {
		return nil, apperrors.New(413, "media_too_large", "Upload exceeds 10 MiB")
	}
	return b, err
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(b)
}
func verify(u db.AppMediaUpload, b []byte) (string, int, int, error) {
	if !u.ExpectedSize.Valid || int64(len(b)) != u.ExpectedSize.Int64 {
		return "", 0, 0, invalid("Content size differs from reservation")
	}
	h := sha256.Sum256(b)
	if !bytes.Equal(h[:], u.ExpectedSha256) {
		return "", 0, 0, invalid("Content hash differs from reservation")
	}
	return inspect(b, declared(u))
}
func usable(u db.AppMediaUpload, expired bool) error {
	// Infinity cannot be represented in the receipt (pgx drops it to year 1), so it is rejected.
	if expired || !u.ExpiresAt.Valid || u.ExpiresAt.InfinityModifier != pgtype.Finite {
		return conflict("Upload session has expired")
	}
	if u.Status != "reserved" && u.Status != "stored" {
		return conflict("Upload session does not accept content")
	}
	return nil
}
func (s *Service) PutMediaUploadContent(ctx context.Context, r httpapi.PutMediaUploadContentRequestObject) (httpapi.PutMediaUploadContentResponseObject, error) {
	// Read the size-capped body first so a slow client never holds a pooled
	// connection or the upload row lock while streaming.
	b, err := readContent(ctx, r.Body)
	if err != nil {
		return nil, err
	}
	err = s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		u, e := q.LockMediaUpload(ctx, db.LockMediaUploadParams{TenantID: dbID(r.TenantId), ID: dbID(r.UploadId)})
		if e != nil {
			return e
		}
		expired, e := q.MediaUploadExpired(ctx, db.MediaUploadExpiredParams{TenantID: dbID(r.TenantId), ID: dbID(r.UploadId)})
		if e != nil {
			return e
		}
		if e = usable(u, expired); e != nil {
			return e
		}
		if _, _, _, e = verify(u, b); e != nil {
			return e
		}
		blob, e := s.store.Put(ctx, bytes.NewReader(b))
		if e != nil {
			return e
		}
		return q.StoreMediaUploadContent(ctx, db.StoreMediaUploadContentParams{TenantID: dbID(r.TenantId), ID: dbID(r.UploadId), TemporaryKey: pgtype.Text{String: blob.Key, Valid: true}, ReceivedBytes: blob.Size})
	})
	if err != nil {
		return nil, err
	}
	return httpapi.PutMediaUploadContent204Response{}, nil
}
func (s *Service) CompleteMediaUpload(ctx context.Context, r httpapi.CompleteMediaUploadRequestObject) (httpapi.CompleteMediaUploadResponseObject, error) {
	var result db.AppMediaUpload
	err := s.transaction(ctx, r.TenantId, func(q *db.Queries) error {
		u, e := q.LockMediaUpload(ctx, db.LockMediaUploadParams{TenantID: dbID(r.TenantId), ID: dbID(r.UploadId)})
		if e != nil {
			return e
		}
		expired, e := q.MediaUploadExpired(ctx, db.MediaUploadExpiredParams{TenantID: dbID(r.TenantId), ID: dbID(r.UploadId)})
		if e != nil {
			return e
		}
		if u.Status == "ready" {
			if !u.MediaID.Valid {
				return conflict("Completed asset was deleted")
			}
			result = u
			return nil
		}
		if e = usable(u, expired); e != nil {
			return e
		}
		if u.Status != "stored" || !u.TemporaryKey.Valid {
			return conflict("Upload content has not been stored")
		}
		f, e := s.store.Open(u.TemporaryKey.String)
		if e != nil {
			return conflict("Stored content is unavailable")
		}
		defer f.Close()
		b, e := readContent(ctx, f)
		if e != nil {
			return e
		}
		mime, w, h, e := verify(u, b)
		if e != nil {
			return e
		}
		blob, e := q.UpsertReadyMediaBlob(ctx, db.UpsertReadyMediaBlobParams{TenantID: dbID(r.TenantId), StorageKey: u.TemporaryKey.String, Sha256: u.ExpectedSha256, SizeBytes: int64(len(b))})
		if errors.Is(e, pgx.ErrNoRows) {
			// Same content is being deleted; never revive it. Retry after deletion.
			return conflict("Identical content is being deleted; retry completion later")
		}
		if e != nil {
			return e
		}
		asset, e := q.CreateMediaAsset(ctx, db.CreateMediaAssetParams{TenantID: dbID(r.TenantId), BlobID: blob.ID, Filename: u.OriginalFilename, MimeType: mime, Width: pgtype.Int4{Int32: int32(w), Valid: true}, Height: pgtype.Int4{Int32: int32(h), Valid: true}})
		if e != nil {
			return e
		}
		if e = q.CompleteMediaUploadRecord(ctx, db.CompleteMediaUploadRecordParams{TenantID: dbID(r.TenantId), ID: dbID(r.UploadId), MediaID: asset.ID}); e != nil {
			return e
		}
		u.Status = "ready"
		u.MediaID = asset.ID
		result = u
		return nil
	})
	if err != nil {
		return nil, err
	}
	return httpapi.CompleteMediaUpload202JSONResponse(session(result)), nil
}
