// HTTP admission tests exercise real generated routes and the private tenant
// stamp; no fake SQL claims are made. PostgreSQL behavior lives in pgx tests.
package media

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
)

type allowMedia struct{}

func (allowMedia) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	return ctx, nil
}
func (allowMedia) ResolveActor(context.Context, *http.Request, httpapi.RouteSpec) (httpapi.Actor, error) {
	return httpapi.Actor{Kind: "user", ID: "33333333-3333-4333-8333-333333333333"}, nil
}
func mediaRouter(t *testing.T, s *Service) *httpapi.Router {
	t.Helper()
	r := httpapi.New(allowMedia{}, nil)
	for _, route := range s.Routes() {
		if e := r.Register(route.Spec, route.Handler); e != nil {
			t.Fatal(e)
		}
	}
	return r
}
func TestMediaTrustAndAdmission(t *testing.T) {
	calls := 0
	s := New(Config{Root: t.TempDir(), Run: func(_ context.Context, _ string, fn func(*db.Queries) error) error { calls++; return fn(nil) }})
	id := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	if _, e := s.CreateMediaUpload(context.Background(), generated.CreateMediaUploadRequestObject{TenantId: id}); e == nil || calls != 0 {
		t.Fatal("unstamped call reached persistence")
	}
	router := mediaRouter(t, s)
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"filename":"../x.png","size_bytes":1,"sha256":"` + strings.Repeat("a", 64) + `","declared_mime":"image/png"}`, 422},
		{`{"filename":"x.svg","size_bytes":1,"sha256":"` + strings.Repeat("a", 64) + `","declared_mime":"image/svg+xml"}`, 415},
		{`{"filename":"x.png","size_bytes":10485761,"sha256":"` + strings.Repeat("a", 64) + `"}`, 413},
		{`{"filename":"x.png","size_bytes":1,"sha256":"wrong"}`, 400},
		{`{"filename":"x.png","size_bytes":1,"sha256":"` + strings.Repeat("a", 64) + `","extra":true}`, 400},
	} {
		req := httptest.NewRequest("POST", "/api/v1/t/"+id.String()+"/media/uploads", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "bf281518-de88-5533-8606-223fc79db7fb")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("body=%s got %d want %d: %s", tc.body, w.Code, tc.want, w.Body.String())
		}
	}
}

// eofReader records whether the request body was fully consumed.
type eofReader struct {
	r    *strings.Reader
	done *bool
}

func (e eofReader) Read(b []byte) (int, error) {
	n, err := e.r.Read(b)
	if err != nil {
		*e.done = true
	}
	return n, err
}

// A slow client must not hold a pooled connection or upload row lock: the
// bounded body is read before the tenant transaction is opened.
func TestPutReadsBodyBeforeTransaction(t *testing.T) {
	read, readFirst := false, false
	s := New(Config{Root: t.TempDir(), Run: func(_ context.Context, _ string, _ func(*db.Queries) error) error {
		readFirst = read
		return errors.New("stop")
	}})
	router := mediaRouter(t, s)
	id := "11111111-1111-4111-8111-111111111111"
	req := httptest.NewRequest("PUT", "/api/v1/t/"+id+"/media/uploads/"+id+"/content", eofReader{strings.NewReader("bytes"), &read})
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Idempotency-Key", "7a59aeb7-287a-58f1-800c-007e4a507988")
	router.ServeHTTP(httptest.NewRecorder(), req)
	if !readFirst {
		t.Fatal("transaction opened before the upload body was read")
	}
}

func TestConfiguredInvalidRootFailsStartup(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if e := os.WriteFile(file, nil, 0o600); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("invalid configured media root did not stop startup")
		}
	}()
	New(Config{Root: filepath.Join(file, "media")})
}
