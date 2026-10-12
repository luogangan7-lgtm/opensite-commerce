package main

import (
	"bytes"
	"encoding/json"
	"example.com/opensite-commerce/internal/storage/local"
	"net/http"
	"net/http/httptest"
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
