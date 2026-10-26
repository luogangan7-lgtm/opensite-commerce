// Package local is a content-addressed, free, self-hosted filesystem BlobStore.
// SQL metadata and access control are deliberately outside this package.
// Called by cmd/dev-api and storage tests; uses only the standard filesystem APIs.
// New/Put/Remove acknowledge success only after affected directories are synced.
// Never expose raw keys in a public URL; use an authorized media service.
// privacy-artifact: media.originals
// privacy-artifact: media.staging
package local

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Blob struct {
	Key    string
	SHA256 string
	Size   int64
}
type Store struct {
	root     string
	maxBytes int64
	syncDir  func(string) error
}

func New(root string, maxBytes int64) (*Store, error) {
	return newWithSyncDir(root, maxBytes, syncDir)
}

// newWithSyncDir keeps durability fault injection local to one store.
func newWithSyncDir(root string, maxBytes int64, syncDir func(string) error) (*Store, error) {
	if maxBytes < 1 {
		return nil, errors.New("maxBytes must be positive")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Deepest ancestor that already exists: every directory below it is created
	// here and must have its entry fsynced in its parent.
	existing := abs
	for {
		if _, err := os.Stat(existing); err == nil || filepath.Dir(existing) == existing {
			break
		}
		existing = filepath.Dir(existing)
	}
	for _, path := range []string{abs, filepath.Join(abs, "staging"), filepath.Join(abs, "originals")} {
		if err = os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
	}
	// Make the staging/ and originals/ entries (and root itself) durable once, so
	// Put's per-upload fsync chain can stop at originals/.
	for dir := abs; ; dir = filepath.Dir(dir) {
		if err = syncDir(dir); err != nil {
			return nil, err
		}
		if dir == existing || filepath.Dir(dir) == dir {
			break
		}
	}
	return &Store{root: abs, maxBytes: maxBytes, syncDir: syncDir}, nil
}

// Put writes to a private staging file on the same filesystem, then atomically
// renames a verified content-addressed blob into place. The caller must mark
// media READY only after both file and SQL metadata are committed/reconciled.
func (s *Store) Put(ctx context.Context, src io.Reader) (Blob, error) {
	var zero Blob
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	f, err := os.CreateTemp(filepath.Join(s.root, "staging"), "upload-*")
	if err != nil {
		return zero, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return zero, err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(ctxReader{ctx, src}, s.maxBytes+1))
	if err != nil {
		f.Close()
		return zero, err
	}
	if n > s.maxBytes {
		f.Close()
		return zero, fmt.Errorf("blob exceeds %d bytes", s.maxBytes)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return zero, err
	}
	if err := f.Close(); err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	sha := hex.EncodeToString(hash.Sum(nil))
	key := filepath.Join(sha[:2], sha[2:4], sha)
	dst := filepath.Join(s.root, "originals", key)
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return zero, err
	}
	// Content-addressed keys prevent distinct content collisions. Concurrent
	// same-content uploads may both rename; bytes are identical by SHA-256.
	if err := os.Rename(tmp, dst); err != nil {
		return zero, err
	}
	// Persist removal of the staging entry so no orphan upload-* survives a crash.
	if err := s.syncDir(filepath.Join(s.root, "staging")); err != nil {
		return zero, err
	}
	// Persist the rename and any hash-prefix directories MkdirAll just created:
	// leaf (bb/), its parent (aa/) and originals/ itself.
	for dir := filepath.Dir(dst); ; dir = filepath.Dir(dir) {
		if err := s.syncDir(dir); err != nil {
			return zero, err
		}
		if dir == filepath.Join(s.root, "originals") {
			break
		}
	}
	return Blob{Key: filepath.ToSlash(key), SHA256: sha, Size: n}, nil
}

// ctxReader stops a copy between reads once ctx is done.
// ponytail: cannot interrupt a single Read that blocks forever; callers must
// pass a reader that unblocks on cancellation (net/http request bodies do).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func (s *Store) Open(key string) (*os.File, error) {
	if !validKey(key) {
		return nil, errors.New("invalid blob key")
	}
	return os.Open(filepath.Join(s.root, "originals", filepath.FromSlash(key)))
}
func (s *Store) Exists(key string) (bool, error) {
	if !validKey(key) {
		return false, errors.New("invalid blob key")
	}
	_, err := os.Stat(filepath.Join(s.root, "originals", filepath.FromSlash(key)))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Remove MUST be called only after Media Service checks ALL DB references,
// active releases and retention rules. It is intentionally not exported via HTTP.
func (s *Store) Remove(key string) error {
	if !validKey(key) {
		return errors.New("invalid blob key")
	}
	path := filepath.Join(s.root, "originals", filepath.FromSlash(key))
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Retry the directory sync even after an earlier unlink already succeeded.
	// A missing hash dir means the key never existed there: sync the nearest
	// existing ancestor (originals always exists) and keep Remove idempotent.
	originals := filepath.Join(s.root, "originals")
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		err = s.syncDir(dir) // make the acknowledged delete durable
		if !errors.Is(err, os.ErrNotExist) || dir == originals {
			return err
		}
	}
}
func validKey(key string) bool {
	if len(key) != 2+1+2+1+64 {
		return false
	}
	p := strings.Split(key, "/")
	if len(p) != 3 || len(p[0]) != 2 || len(p[1]) != 2 || len(p[2]) != 64 || p[0] != p[2][:2] || p[1] != p[2][2:4] {
		return false
	}
	for _, c := range p[2] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
