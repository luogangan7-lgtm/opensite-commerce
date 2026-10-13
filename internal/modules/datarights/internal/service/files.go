// Private export artifacts and HMAC capabilities. Uses os.Root to confine every
// access, rejects symlinks, bounds bytes, writes 0600 atomically with fsync.
// Called only by data-rights service while its customer/request DB locks are held.
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
	d, e := r.Open(filepath.Dir(p))
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
	return nil
}
