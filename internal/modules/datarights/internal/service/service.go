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
	"errors"
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
	// Optional directory-sync observer for storage fault and durability probes.
	syncDir func(*os.Root, string) error
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
	var meta struct {
		contract.Request
		TokenHash         string `json:"token_hash"`
		DownloadAvailable bool   `json:"download_available"`
	}
	if e = decode(b, e, &meta); e != nil {
		return out, e
	}
	out = meta.Request
	if out.Status == "ready" && meta.DownloadAvailable {
		token := s.token(tenant, id, out.DownloadExpiresAt)
		hash := sha256.Sum256([]byte(token))
		stored, err := hex.DecodeString(meta.TokenHash)
		if err == nil && subtle.ConstantTimeCompare(hash[:], stored) == 1 {
			out.DownloadToken = token
		}
	}
	return out, nil
}

func exportTerminalError(status string) error {
	switch status {
	case "failed":
		return contract.ErrInvalid
	case "expired", "consumed", "revoked":
		return contract.ErrConflict
	}
	return nil
}

func (s *Service) BuildExport(ctx context.Context, tenant, id string) error {
	if s.run == nil {
		return contract.ErrUnavailable
	}
	if _, e := store.UUID(tenant); e != nil {
		return e
	}
	r, e := store.UUID(id)
	if e != nil {
		return e
	}
	var terminal error
	e = s.run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q, e := store.Tenant(ctx, tx, tenant)
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
		// Terminal metadata can be written by this query. Commit it before
		// returning an error that tells the queue to cancel the job.
		terminal = exportTerminalError(existing.Status)
		if terminal != nil {
			return nil
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
		if existing.Status != "pending" {
			return contract.ErrUnavailable
		}
		b, e := q.DataRightsBuildExport(ctx, r)
		if e != nil {
			return store.MapError(e)
		}
		if len(b) > MaxBytes {
			return contract.ErrUnavailable
		}
		// Build query holds customer+request locks. Metadata status is read-only.
		var meta struct {
			Status string `json:"status"`
			Customer struct {
				ID string `json:"id"`
			} `json:"customer"`
		}
		if json.Unmarshal(b, &meta) != nil {
			return contract.ErrUnavailable
		}
		if meta.Status != "" {
			terminal = exportTerminalError(meta.Status)
			if terminal == nil {
				return contract.ErrUnavailable
			}
			return nil
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
	if e != nil {
		// A deadline can pass after worker-info but before completion. A
		// rolled-back transition must remain retryable until SQL commits it.
		if errors.Is(e, contract.ErrInvalid) || errors.Is(e, contract.ErrConflict) {
			return contract.ErrUnavailable
		}
		return e
	}
	return terminal
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
