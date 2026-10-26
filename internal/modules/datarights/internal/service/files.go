// Private export artifacts and HMAC capabilities. Uses os.Root to confine every
// access, rejects symlinks, bounds bytes, writes 0600 atomically with fsync.
// Called by data-rights service under customer/request locks; the periodic
// sweeper shares its export-ID advisory lock with construction.
package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"github.com/google/uuid"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/internal/store"
	"time"
)

const MaxBytes = contract.MaxBytes

func validID(id string) bool {
	u, e := uuid.Parse(id)
	return e == nil && u != uuid.Nil && u.String() == id
}
func (s *Service) path(tenant, id string) (string, error) {
	if !validID(tenant) || !validID(id) {
		return "", contract.ErrInvalid
	}
	return filepath.Join(s.root, "data-rights", tenant, id+".json"), nil
}
func (s *Service) token(tenant, id string, expires time.Time) string {
	h := hmac.New(sha256.New, s.key)
	fmt.Fprintf(h, "data-rights/v1\n%s\n%s\n%s", tenant, id, strconv.FormatInt(expires.Unix(), 10))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func (s *Service) openRoot() (*os.Root, error) { return os.OpenRoot(s.root) }
func (s *Service) relative(tenant, id string) (string, error) {
	if _, e := s.path(tenant, id); e != nil {
		return "", e
	}
	return filepath.Join("data-rights", tenant, id+".json"), nil
}
func (s *Service) write(tenant, id string, b []byte) error {
	if len(b) > MaxBytes {
		return contract.ErrInvalid
	}
	p, e := s.relative(tenant, id)
	if e != nil {
		return e
	}
	r, e := s.openRoot()
	if e != nil {
		return e
	}
	defer r.Close()
	if e = r.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return e
	}
	// os.Root confines symlink resolution; refuse even in-root directory symlinks.
	for _, dir := range []string{"data-rights", filepath.Join("data-rights", tenant)} {
		st, e := r.Lstat(dir)
		if e != nil {
			return e
		}
		if !st.IsDir() || st.Mode().Perm() != 0700 {
			return contract.ErrUnavailable
		}
	}
	// Persist both directory entries before publishing a file in the tenant
	// directory. Repeat on retries: a previous creation may have outlived fsync.
	for _, dir := range []string{".", "data-rights"} {
		if e = s.syncDirectory(r, dir); e != nil {
			return e
		}
	}
	tmp := p + "." + uuid.NewString() + ".tmp"
	f, e := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer r.Remove(tmp)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = r.Rename(tmp, p); e != nil {
		return e
	}
	return s.syncDirectory(r, filepath.Dir(p))
}
func (s *Service) syncDirectory(r *os.Root, path string) error {
	if s.syncDir != nil {
		return s.syncDir(r, path)
	}
	d, e := r.Open(path)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (s *Service) read(tenant, id string) ([]byte, error) {
	p, e := s.relative(tenant, id)
	if e != nil {
		return nil, e
	}
	r, e := s.openRoot()
	if e != nil {
		return nil, e
	}
	defer r.Close()
	st, e := r.Lstat(p)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() > MaxBytes {
		return nil, contract.ErrUnavailable
	}
	f, e := r.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	actual, e := f.Stat()
	if e != nil || !os.SameFile(st, actual) {
		return nil, contract.ErrUnavailable
	}
	b, e := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if e != nil {
		return nil, e
	}
	if len(b) > MaxBytes {
		return nil, contract.ErrUnavailable
	}
	return b, nil
}
func (s *Service) Cleanup(ctx context.Context, tenant string, ids []string) error {
	if !validID(tenant) {
		return contract.ErrInvalid
	}
	r, e := s.openRoot()
	if e != nil {
		return contract.ErrUnavailable
	}
	defer r.Close()
	for _, id := range ids {
		if e = ctx.Err(); e != nil {
			return e
		}
		p, e := s.relative(tenant, id)
		if e != nil {
			return e
		}
		if e = r.Remove(p); e != nil && !os.IsNotExist(e) {
			return contract.ErrUnavailable
		}
	}
	// Sync even on a retry where every unlink reports ENOENT.
	d, e := r.Open(filepath.Join("data-rights", tenant))
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return contract.ErrUnavailable
	}
	defer d.Close()
	return d.Sync()
}

// Sweep removes private artifact copies without a committed live capability.
// Called by the signed periodic tenant worker. The DB advisory lock also covers
// pending/missing rows and remains held through unlink + directory fsync.
func (s *Service) Sweep(ctx context.Context, tenant string) error {
	if !validID(tenant) || s.run == nil {
		return contract.ErrInvalid
	}
	r, err := s.openRoot()
	if err != nil {
		return contract.ErrUnavailable
	}
	defer r.Close()
	dir := filepath.Join("data-rights", tenant)
	for _, name := range []string{"data-rights", dir} {
		st, err := r.Lstat(name)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil || !st.IsDir() || st.Mode().Perm() != 0700 {
			return contract.ErrUnavailable
		}
	}
	d, err := r.Open(dir)
	if err != nil {
		return contract.ErrUnavailable
	}
	defer d.Close()
	for {
		entries, readErr := d.ReadDir(128)
		if readErr != nil && readErr != io.EOF {
			return contract.ErrUnavailable
		}
		for _, entry := range entries {
			parts := strings.Split(entry.Name(), ".")
			final := len(parts) == 2 && parts[1] == "json"
			temp := len(parts) == 4 && parts[1] == "json" && validID(parts[2]) && parts[3] == "tmp"
			if (!final && !temp) || !validID(parts[0]) {
				continue
			}
			err = s.run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
				q, err := store.Tenant(ctx, tx, tenant)
				if err != nil {
					return err
				}
				id, err := store.UUID(parts[0])
				if err != nil {
					return err
				}
				live, err := q.DataRightsArtifactLive(ctx, id)
				if err != nil {
					return store.MapError(err)
				}
				if final && live {
					return nil
				}
				if err := r.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
					return contract.ErrUnavailable
				}
				return d.Sync()
			})
			if err != nil {
				return err
			}
		}
		// A previous unlink may have succeeded before its fsync failed.
		// Sync an empty retry too, before acknowledging reclamation.
		if readErr == io.EOF {
			return d.Sync()
		}
	}
}
