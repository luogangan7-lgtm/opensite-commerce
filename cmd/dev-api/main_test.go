package main

import (
	"bytes"
	"encoding/json"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/storage/local"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDiagnosticAPI(t *testing.T) {
	store, _ := local.New(t.TempDir(), 1024)
	handler := newHandler(store)
	good := []byte(`{"schema_version":1,"root_id":"root","nodes":{"root":{"component":"core.page-layout@1","props":{},"slots":{"main":[]}}}}`)
	req := httptest.NewRequest(http.MethodPost, "/internal/preflight/page", bytes.NewReader(good))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("valid page: %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/internal/preflight/page", bytes.NewReader([]byte(`{"schema_version":99}`)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 422 {
		t.Fatalf("invalid page: %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/internal/preflight/blob", bytes.NewReader([]byte("hello")))
	req.Header.Set("Content-Type", "application/octet-stream")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	var r reply
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	if r.Key == "" || r.SHA256 == "" || r.Bytes != 5 {
		t.Fatalf("bad blob response: %#v", r)
	}
}

func TestDevMediaRootRejectsFilesystemRoot(t *testing.T) {
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(string(filepath.Separator), link); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"/", "/./", "//", link} {
		t.Run(root, func(t *testing.T) {
			if _, err := devMediaRoot(root); err == nil || err.Error() != "refusing to use filesystem root" {
				t.Fatalf("filesystem root %q: got %v", root, err)
			}
		})
	}
}

func TestDevMediaRootResolvesExistingAncestor(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "media-link")
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "new", "media")
	got, err := devMediaRoot(filepath.Join(link, "new", "media"))
	if err != nil || got != want {
		t.Fatalf("root=%q err=%v, want %q", got, err, want)
	}
	if _, err := local.New(got, 1024); err != nil {
		t.Fatal(err)
	}
}
