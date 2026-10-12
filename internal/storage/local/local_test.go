package local

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPutOpenAndDedup(t *testing.T) {
	s, e := New(t.TempDir(), 1024)
	if e != nil {
		t.Fatal(e)
	}
	src := []byte("image bytes for the local store")
	a, e := s.Put(context.Background(), bytes.NewReader(src))
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.Put(context.Background(), bytes.NewReader(src))
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(src)
	if a.SHA256 != hex.EncodeToString(h[:]) || a.Key != b.Key {
		t.Fatal("hash mismatch or failed dedup")
	}
	f, e := s.Open(a.Key)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	data, e := io.ReadAll(f)
	if e != nil || !bytes.Equal(src, data) {
		t.Fatal("blob altered", e)
	}
}

func TestNewSyncsEveryCreatedAncestor(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "a", "b", "c")
	want := []string{root, filepath.Dir(root), filepath.Join(base, "a"), base}
	var synced []string
	_, err := newWithSyncDir(root, 16, func(path string) error {
		synced = append(synced, path)
		return syncDir(path)
	})
	if err != nil || !reflect.DeepEqual(synced, want) {
		t.Fatalf("synced=%v err=%v, want %v", synced, err, want)
	}
}

func TestNewPropagatesAncestorSyncFailure(t *testing.T) {
	for depth := 0; depth < 4; depth++ {
		t.Run(fmt.Sprintf("depth-%d", depth), func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "a", "b", "c")
			want := []string{root, filepath.Dir(root), filepath.Join(base, "a"), base}
			fault := errors.New("ancestor sync failed")
			var synced []string
			s, err := newWithSyncDir(root, 16, func(path string) error {
				synced = append(synced, path)
				if path == want[depth] {
					return fault
				}
				return syncDir(path)
			})
			if s != nil || !errors.Is(err, fault) || !reflect.DeepEqual(synced, want[:depth+1]) {
				t.Fatalf("store=%v synced=%v err=%v", s, synced, err)
			}
		})
	}
}

func TestPutSyncsStagingAndHashAncestors(t *testing.T) {
	s, err := New(t.TempDir(), 16)
	if err != nil {
		t.Fatal(err)
	}
	var synced []string
	s.syncDir = func(path string) error {
		synced = append(synced, path)
		return syncDir(path)
	}
	blob, err := s.Put(context.Background(), strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Dir(filepath.Join(s.root, "originals", filepath.FromSlash(blob.Key)))
	want := []string{filepath.Join(s.root, "staging"), leaf, filepath.Dir(leaf), filepath.Join(s.root, "originals")}
	if !reflect.DeepEqual(synced, want) {
		t.Fatalf("synced=%v, want %v", synced, want)
	}
}

func TestRemoveRetriesDirectorySyncAfterUnlink(t *testing.T) {
	s, err := New(t.TempDir(), 16)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := s.Put(context.Background(), strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.root, "originals", filepath.FromSlash(blob.Key))
	fault := errors.New("delete directory sync failed")
	calls := 0
	s.syncDir = func(dir string) error {
		calls++
		if dir != filepath.Dir(path) {
			t.Fatalf("synced %q instead of blob directory", dir)
		}
		if calls <= 2 {
			return fault
		}
		return syncDir(dir)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		err := s.Remove(blob.Key)
		if calls != attempt || (attempt <= 2 && !errors.Is(err, fault)) || (attempt == 3 && err != nil) {
			t.Fatalf("attempt=%d sync calls=%d err=%v", attempt, calls, err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("blob still exists after unlink: %v", err)
		}
	}
}
func TestOversize(t *testing.T) {
	s, _ := New(t.TempDir(), 4)
	if _, e := s.Put(context.Background(), strings.NewReader("12345")); e == nil {
		t.Fatal("oversize accepted")
	}
}
func TestTraversal(t *testing.T) {
	s, _ := New(t.TempDir(), 4)
	if _, e := s.Open("../../etc/passwd"); e == nil {
		t.Fatal("traversal accepted")
	}
}
func TestCancelled(t *testing.T) {
	s, _ := New(t.TempDir(), 4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := s.Put(ctx, strings.NewReader("x")); !errors.Is(e, context.Canceled) {
		t.Fatalf("want cancellation, got %v", e)
	}
}

func TestMalformedKeyDoesNotPanic(t *testing.T) {
	s, _ := New(t.TempDir(), 1024)
	key := strings.Repeat("a", 64) + "/b/c"
	if _, err := s.Open(key); err == nil {
		t.Fatal("invalid blob key accepted")
	}
}

// cancelAfterFirst cancels the context after its first chunk, then keeps
// supplying data forever; Put must stop instead of reading to maxBytes.
type cancelAfterFirst struct {
	cancel context.CancelFunc
	reads  int
}

func (r *cancelAfterFirst) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 1 {
		r.cancel()
	}
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestCancelledMidCopy(t *testing.T) {
	s, _ := New(t.TempDir(), 1<<30)
	ctx, cancel := context.WithCancel(context.Background())
	r := &cancelAfterFirst{cancel: cancel}
	if _, err := s.Put(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if r.reads > 2 {
		t.Fatalf("kept reading after cancellation: %d reads", r.reads)
	}
}

func TestNewCreatesNestedRoot(t *testing.T) {
	root := t.TempDir() + "/a/b/c" // several new levels; New must create and sync them
	s, err := New(root, 16)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveNeverStoredKeyIsIdempotent(t *testing.T) {
	key := "00/00/" + strings.Repeat("0", 64)
	for _, tc := range []struct {
		name string
		mk   string // hash dir that already exists under originals, if any
	}{{"empty store", ""}, {"only first-level dir", "00"}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			s, err := New(root, 16)
			if err != nil {
				t.Fatal(err)
			}
			if tc.mk != "" {
				if err := os.Mkdir(filepath.Join(root, "originals", tc.mk), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				if err := s.Remove(key); err != nil {
					t.Fatalf("Remove #%d: %v", i+1, err)
				}
			}
		})
	}
}
