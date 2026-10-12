//go:build pgx

// PostgreSQL HTTP gates run the actual sqlc queries as app_media_service through
// tenanttx. A disposable root-provisioned database owns fixture lifetime; tests
// never open raw pools, mutate roles, or issue raw business SQL.
package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

const mediaTenant = "11111111-1111-4111-8111-111111111111"
const mediaOtherTenant = "22222222-2222-4222-8222-222222222222"
const mediaProduct = "33333333-3333-4333-8333-333333333333"
const mediaPinned = "55555555-5555-4555-8555-555555555555"

type uploadReceipt struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	MediaID   *string   `json:"media_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func TestMediaPostgresHTTP(t *testing.T) {
	dsn := os.Getenv("C07_MEDIA_DATABASE_URL")
	if dsn == "" {
		t.Skip("NOT_RUN: C07_MEDIA_DATABASE_URL not configured")
	}
	parsed, err := url.Parse(dsn)
	// The disposable login inherits only app_media_service; URI query user must
	// not override that login and silently turn least-privilege proof into admin.
	if err != nil || parsed.User == nil || parsed.User.Username() != "c07_media" || parsed.Query().Has("user") {
		t.Fatal("C07_MEDIA_DATABASE_URL must login as the dedicated c07_media fixture role without user override")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	runner, e := pgxadapter.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer runner.Close()
	root := t.TempDir()
	srv := httptest.NewServer(mediaRouter(t, New(Config{Run: runner.Queries, Root: root})))
	defer srv.Close()
	call := func(method, tenant, path, content string, body io.Reader, want int) []byte {
		t.Helper()
		req, e := http.NewRequestWithContext(ctx, method, srv.URL+"/api/v1/t/"+tenant+path, body)
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Content-Type", content)
		req.Header.Set("Idempotency-Key", "media-gate")
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, e := io.ReadAll(res.Body)
		if e != nil {
			t.Fatal(e)
		}
		if res.StatusCode != want {
			t.Fatalf("%s %s got=%d want=%d body=%s", method, path, res.StatusCode, want, b)
		}
		return b
	}
	receipt := func(b []byte) uploadReceipt {
		t.Helper()
		var u uploadReceipt
		if e := json.Unmarshal(b, &u); e != nil {
			t.Fatal(e)
		}
		return u
	}
	reserve := func(tenant, name, mime string, b []byte) uploadReceipt {
		t.Helper()
		h := sha256.Sum256(b)
		payload, _ := json.Marshal(map[string]any{"filename": name, "size_bytes": len(b), "sha256": hex.EncodeToString(h[:]), "declared_mime": mime})
		u := receipt(call("POST", tenant, "/media/uploads", "application/json", bytes.NewReader(payload), 201))
		if u.Status != "reserved" || u.MediaID != nil || !u.ExpiresAt.After(time.Now()) {
			t.Fatalf("reservation %+v", u)
		}
		return u
	}
	data := pngBytes(t)
	u := reserve(mediaTenant, "photo.png", "image/png", data)
	// Same URL identifiers under a different tenant cannot observe the row.
	call("PUT", mediaOtherTenant, "/media/uploads/"+u.ID+"/content", "application/octet-stream", bytes.NewReader(data), 404)
	call("POST", mediaOtherTenant, "/media/uploads/"+u.ID+"/complete", "application/json", nil, 404)
	call("POST", mediaTenant, "/media/uploads/"+u.ID+"/complete", "application/json", nil, 409)
	// Actual stream size/hash, not Content-Length or client declarations, wins.
	call("PUT", mediaTenant, "/media/uploads/"+u.ID+"/content", "application/octet-stream", bytes.NewReader(bytes.Repeat([]byte{1}, int(MaxBytes+1))), 413)
	changed := append([]byte(nil), data...)
	changed[len(changed)-1] ^= 1
	call("PUT", mediaTenant, "/media/uploads/"+u.ID+"/content", "application/octet-stream", bytes.NewReader(changed), 422)
	call("PUT", mediaTenant, "/media/uploads/"+u.ID+"/content", "application/octet-stream", bytes.NewReader(data[:len(data)-1]), 422)
	call("PUT", mediaTenant, "/media/uploads/"+u.ID+"/content", "application/octet-stream", bytes.NewReader(data), 204)
	done := receipt(call("POST", mediaTenant, "/media/uploads/"+u.ID+"/complete", "application/json", nil, 202))
	if done.Status != "ready" || done.MediaID == nil {
		t.Fatalf("completion %+v", done)
	}
	// Concurrent completion is serialized by FOR UPDATE and must keep one ID.
	var wg sync.WaitGroup
	type replayResult struct {
		body   []byte
		status int
		err    error
	}
	replays := make(chan replayResult, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, e := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/v1/t/"+mediaTenant+"/media/uploads/"+u.ID+"/complete", nil)
			if e != nil {
				replays <- replayResult{err: e}
				return
			}
			req.Header.Set("Idempotency-Key", "media-replay")
			res, e := http.DefaultClient.Do(req)
			if e != nil {
				replays <- replayResult{err: e}
				return
			}
			defer res.Body.Close()
			b, e := io.ReadAll(res.Body)
			replays <- replayResult{b, res.StatusCode, e}
		}()
	}
	wg.Wait()
	close(replays)
	for r := range replays {
		if r.err != nil || r.status != 202 {
			t.Fatalf("replay status=%d err=%v body=%s", r.status, r.err, r.body)
		}
		again := receipt(r.body)
		if again.MediaID == nil || *again.MediaID != *done.MediaID {
			t.Fatalf("unstable completion %+v", again)
		}
	}
	for _, tenant := range []string{mediaTenant, mediaOtherTenant} {
		d := reserve(tenant, "duplicate.png", "image/png", data)
		call("PUT", tenant, "/media/uploads/"+d.ID+"/content", "application/octet-stream", bytes.NewReader(data), 204)
		receipt(call("POST", tenant, "/media/uploads/"+d.ID+"/complete", "application/json", nil, 202))
	}
	n := 0
	if e := filepath.WalkDir(filepath.Join(root, "originals"), func(_ string, d fs.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			n++
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if n != 1 {
		t.Fatalf("content dedup wrote %d physical files", n)
	}
	for _, bad := range []struct {
		mime string
		data []byte
	}{
		{"image/png", []byte("\x89PNG\r\n\x1a\nfake")}, {"image/webp", []byte("RIFF\x10\x00\x00\x00WEBPVP8 fake")}, {"image/avif", []byte("\x00\x00\x00\x18ftypavif\x00\x00\x00\x00avifmif1")}, {"image/png", []byte("<svg></svg>")},
	} {
		d := reserve(mediaTenant, "bad.png", bad.mime, bad.data)
		call("PUT", mediaTenant, "/media/uploads/"+d.ID+"/content", "application/octet-stream", bytes.NewReader(bad.data), 415)
	}
	link := func(id string, want int) {
		call("POST", mediaTenant, "/products/"+mediaProduct+"/media", "application/json", strings.NewReader(fmt.Sprintf(`{"media_id":%q,"role":"gallery","sort_order":2}`, id)), want)
	}
	call("DELETE", mediaOtherTenant, "/media/"+*done.MediaID, "application/json", nil, 404)
	link(*done.MediaID, 201)
	call("DELETE", mediaTenant, "/media/"+*done.MediaID, "application/json", nil, 204)
	call("DELETE", mediaTenant, "/media/"+*done.MediaID, "application/json", nil, 404)
	call("POST", mediaTenant, "/media/uploads/"+u.ID+"/complete", "application/json", nil, 409)
	// Root's post-test SQL checks that the failed DELETE rolled back link removal.
	link(mediaPinned, 201)
	call("DELETE", mediaTenant, "/media/"+mediaPinned, "application/json", nil, 409)
	n = 0
	filepath.WalkDir(filepath.Join(root, "originals"), func(_ string, d fs.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			n++
		}
		return e
	})
	if n != 1 {
		t.Fatal("logical deletion removed shared bytes")
	}
}
