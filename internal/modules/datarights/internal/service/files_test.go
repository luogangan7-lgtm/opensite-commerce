// Tests private export storage and capability derivation, independently of DB.
package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const tenant = "11111111-1111-4111-8111-111111111111"
const request = "22222222-2222-4222-8222-222222222222"

func TestPrivateArtifactAndBounds(t *testing.T) {
	root := t.TempDir()
	s := Service{root: root, key: []byte("01234567890123456789012345678901")}
	if err := s.write(tenant, request, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	path, _ := s.path(tenant, request)
	f, _ := os.Stat(path)
	if f.Mode().Perm() != 0600 {
		t.Fatal("public artifact")
	}
	d, _ := os.Stat(filepath.Dir(path))
	if d.Mode().Perm() != 0700 {
		t.Fatal("public directory")
	}
	if _, err := s.path("../bad", request); err == nil {
		t.Fatal("path traversal")
	}
	if err := s.write(tenant, request, make([]byte, MaxBytes+1)); err == nil {
		t.Fatal("unbounded")
	}
	bytes, err := s.read(tenant, request)
	if err != nil || string(bytes) != `{"ok":true}` {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("secret"), 0600)
	os.Remove(path)
	os.Symlink(outside, path)
	if _, err := s.read(tenant, request); err == nil {
		t.Fatal("symlink accepted")
	}
}
func TestTokenBoundToAllScope(t *testing.T) {
	s := Service{key: []byte("01234567890123456789012345678901")}
	e := time.Unix(1800000000, 0)
	a := s.token(tenant, request, e)
	for _, b := range []string{s.token(request, request, e), s.token(tenant, tenant, e), s.token(tenant, request, e.Add(time.Second))} {
		if a == b {
			t.Fatal("token scope omitted")
		}
	}
	if len(a) < 40 {
		t.Fatal("weak token")
	}
}

func TestCleanupRemovesPrivateArtifactAndRetries(t *testing.T) {
	s := Service{root: t.TempDir()}
	if err := s.write(tenant, request, []byte("private copy")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Cleanup(context.Background(), tenant, []string{request}); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := s.path(tenant, request)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("cleanup retained private artifact", err)
	}
}

func TestWriteSyncsNewDirectoryAncestry(t *testing.T) {
	for _, failAt := range []string{"", ".", "data-rights", filepath.Join("data-rights", tenant)} {
		t.Run(failAt, func(t *testing.T) {
			var synced []string
			failure := errors.New("directory sync failed")
			s := Service{root: t.TempDir(), syncDir: func(_ *os.Root, name string) error {
				synced = append(synced, name)
				if name == failAt {
					return failure
				}
				return nil
			}}
			err := s.write(tenant, request, []byte("private copy"))
			if failAt != "" {
				if !errors.Is(err, failure) {
					t.Fatalf("sync failure at %q was lost: %v", failAt, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []string{".", "data-rights", filepath.Join("data-rights", tenant)}
			if !reflect.DeepEqual(synced, want) {
				t.Fatalf("directory durability sequence = %v, want %v", synced, want)
			}
		})
	}
}
