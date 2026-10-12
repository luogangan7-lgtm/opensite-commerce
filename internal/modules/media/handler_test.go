// HTTP admission tests exercise real generated routes and the private tenant
// stamp; no fake SQL claims are made. PostgreSQL behavior lives in pgx tests.
package media

import (
	"context"
	"net/http"
	"net/http/httptest"
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
		req.Header.Set("Idempotency-Key", "media-admission")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("body=%s got %d want %d: %s", tc.body, w.Code, tc.want, w.Body.String())
		}
	}
}
