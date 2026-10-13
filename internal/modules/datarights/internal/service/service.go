// Service orchestrates SQL capabilities and private export files. Depends only
// on generated DB queries via store and contract; HTTP/jobs supply authorization.
// Locks remain held across blob I/O; commit succeeds before bytes are returned.
package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/internal/store"
	"os"
	"path/filepath"
	"time"
)

type Service struct {
	root string
	key  []byte
	run  func(context.Context, string, func(context.Context, pgx.Tx) error) error
}

func New(c contract.Config) (*Service, error) {
	if c.Run == nil || !filepath.IsAbs(c.Root) || len(c.TokenKey) < 32 {
		return nil, contract.ErrInvalid
	}
	st, e := os.Lstat(c.Root)
	if e != nil || !st.IsDir() {
		return nil, contract.ErrInvalid
	}
	return &Service{root: c.Root, key: append([]byte(nil), c.TokenKey...), run: c.Run}, nil
}
func decode(b []byte, e error, out any) error {
	if e != nil {
		return store.MapError(e)
	}
	if json.Unmarshal(b, out) != nil {
		return contract.ErrUnavailable
	}
	return nil
}
func actor(kind, id string) error {
	if kind != "user" && kind != "api_key" && kind != "service" {
		return contract.ErrInvalid
	}
	_, e := store.UUID(id)
	return e
}
func (s *Service) RequestExport(ctx context.Context, tx pgx.Tx, tenant string, in contract.RequestInput) (out contract.Request, e error) {
	q, e := store.Tenant(ctx, tx, tenant)
	if e != nil {
		return out, e
	}
	id, e := store.UUID(in.ID)
	if e != nil {
		return out, e
	}
	c, e := store.UUID(in.CustomerID)
	if e != nil {
		return out, e
	}
	a, e := store.UUID(in.ActorID)
	if e != nil || actor(in.ActorKind, in.ActorID) != nil {
		return out, contract.ErrInvalid
	}
	b, e := q.DataRightsRequestExport(ctx, db.DataRightsRequestExportParams{ID: id, CustomerID: c, ActorKind: in.ActorKind, ActorID: a})
	e = decode(b, e, &out)
	return out, e
}
func (s *Service) Status(ctx context.Context, tx pgx.Tx, tenant, id, customer string) (out contract.Request, e error) {
	q, e := store.Tenant(ctx, tx, tenant)
	if e != nil {
		return out, e
	}
	r, e := store.UUID(id)
	if e != nil {
		return out, e
	}
	c, e := store.UUID(customer)
	if e != nil {
		return out, e
	}
	b, e := q.DataRightsStatus(ctx, db.DataRightsStatusParams{ID: r, CustomerID: c})
	if e = decode(b, e, &out); e != nil {
		return out, e
	}
	if out.Status == "ready" && out.DownloadExpiresAt.After(time.Now()) {
		out.DownloadToken = s.token(tenant, id, out.DownloadExpiresAt)
	}
	return out, nil
}
func (s *Service) BuildExport(ctx context.Context, tenant, id string) error {
	if s.run == nil {
		return contract.ErrUnavailable
	}
	return s.run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q, e := store.Tenant(ctx, tx, tenant)
		if e != nil {
			return e
		}
		r, e := store.UUID(id)
		if e != nil {
			return e
		}
		info, e := q.DataRightsWorkerInfo(ctx, r)
		var existing struct {
			Status      string `json:"status"`
			ContentHash string `json:"content_hash"`
		}
		if e = decode(info, e, &existing); e != nil {
			return e
		}
		if existing.Status == "ready" {
			blob, e := s.read(tenant, id)
			if e != nil {
				return contract.ErrUnavailable
			}
			h := sha256.Sum256(blob)
			if hex.EncodeToString(h[:]) != existing.ContentHash {
				return contract.ErrUnavailable
			}
			return nil
		}
		b, e := q.DataRightsBuildExport(ctx, r)
		if e != nil {
			return store.MapError(e)
		}
		if len(b) > MaxBytes {
			return contract.ErrInvalid
		}
		// Build query holds customer+request locks. Metadata status is read-only.
		var meta struct {
			Customer struct {
				ID string `json:"id"`
			} `json:"customer"`
		}
		if json.Unmarshal(b, &meta) != nil {
			return contract.ErrUnavailable
		}
		_, e = s.Status(ctx, tx, tenant, id, meta.Customer.ID)
		if e != nil {
			return e
		}
		deadline, e := q.DataRightsDownloadDeadline(ctx, r)
		if e != nil {
			return store.MapError(e)
		}
		token := sha256.Sum256([]byte(s.token(tenant, id, deadline.Time)))
		content := sha256.Sum256(b)
		if e = s.write(tenant, id, b); e != nil {
			return contract.ErrUnavailable
		}
		_, e = q.DataRightsCompleteExport(ctx, db.DataRightsCompleteExportParams{ID: r, TokenHash: token[:], ContentHash: content[:], DownloadExpiresAt: deadline})
		return store.MapError(e)
	})
}
func (s *Service) Download(ctx context.Context, tenant, id, customer, token string) (out []byte, e error) {
	if s.run == nil || len(token) > 128 {
		return nil, contract.ErrInvalid
	}
	e = s.run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q, e := store.Tenant(ctx, tx, tenant)
		if e != nil {
			return e
		}
		r, e := store.UUID(id)
		if e != nil {
			return e
		}
		c, e := store.UUID(customer)
		if e != nil {
			return e
		}
		b, e := q.DataRightsDownloadInfo(ctx, db.DataRightsDownloadInfoParams{ID: r, CustomerID: c})
		var meta struct {
			ContentHash       string    `json:"content_hash"`
			DownloadExpiresAt time.Time `json:"download_expires_at"`
		}
		if e = decode(b, e, &meta); e != nil {
			return e
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.token(tenant, id, meta.DownloadExpiresAt))) != 1 {
			return contract.ErrConflict
		}
		out, e = s.read(tenant, id)
		if e != nil {
			return contract.ErrUnavailable
		}
		hash := sha256.Sum256(out)
		if hex.EncodeToString(hash[:]) != meta.ContentHash {
			return contract.ErrUnavailable
		}
		th := sha256.Sum256([]byte(token))
		_, e = q.DataRightsConsumeExport(ctx, db.DataRightsConsumeExportParams{ID: r, CustomerID: c, TokenHash: th[:]})
		return store.MapError(e)
	})
	if e != nil {
		return nil, e
	}
	return out, nil
}
func (s *Service) EraseTx(ctx context.Context, tx pgx.Tx, tenant string, in contract.EraseInput) (out contract.Erasure, e error) {
	q, e := store.Tenant(ctx, tx, tenant)
	if e != nil {
		return out, e
	}
	c, e := store.UUID(in.CustomerID)
	if e != nil {
		return out, e
	}
	a, e := store.UUID(in.ActorID)
	if e != nil || actor(in.ActorKind, in.ActorID) != nil {
		return out, contract.ErrInvalid
	}
	b, e := q.DataRightsEraseCustomer(ctx, db.DataRightsEraseCustomerParams{CustomerID: c, ActorKind: in.ActorKind, ActorID: a})
	e = decode(b, e, &out)
	return out, e
}
func (s *Service) Erase(ctx context.Context, tenant string, in contract.EraseInput) (out contract.Erasure, e error) {
	if s.run == nil {
		return out, contract.ErrUnavailable
	}
	e = s.run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		out, e = s.EraseTx(ctx, tx, tenant, in)
		return e
	})
	if e == nil {
		e = s.Cleanup(ctx, tenant, out.Requests)
	}
	return out, e
}
