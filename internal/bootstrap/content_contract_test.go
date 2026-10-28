// Responsibility: verify catalog content scope and validation in production bootstrap.
// Dependencies: identity role contract via pricingAuth; caller: CI Go admission gate.
// Invariant: existing catalog writers only; malformed/bounded input cannot reach DB.
package bootstrap_test

import (
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaffContentRoles(t *testing.T) {
	base := "/api/v1/t/11111111-1111-4111-8111-111111111111/products/22222222-2222-4222-8222-222222222222"
	for _, name := range []string{"owner", "admin", "catalog_editor", "viewer", "content_editor"} {
		role := pricingAuth(name)
		server, err := bootstrap.New(bootstrap.Config{Authenticator: &role})
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			method, path, body, version string
			want                        int
		}{
			{"GET", base + "/content/en", "", "", 503},
			{"GET", base + "/content/" + strings.Repeat("x", 65), "", "", 422},
			{"PUT", base + "/content/en", `{"title":"copy","body_blocks":[{"text":"` + strings.Repeat("x", 65530) + `"}],"seo":{}}`, `"0"`, 422},
			{"PUT", base + "/content/en", `{"title":"copy","body_blocks":[` + strings.Repeat(`{},`, 128) + `{}],"seo":{}}`, `"0"`, 400},
			{"PUT", base + "/content/en", `{"title":"copy","body_blocks":[],"seo":{}}`, `"0"`, 503},
			{"PUT", base + "/content/en", `{"title":"copy","body_blocks":[],"seo":{}}`, "", 428},
			{"PUT", base + "/content/en", `{"title":"copy","body_blocks":{},"seo":{}}`, `"0"`, 400},
			{"PUT", base + "/content/en", `{"title":"copy","body_blocks":["bad"],"seo":{}}`, `"0"`, 400},
			{"PUT", base + "/content/en", `{"title":"` + strings.Repeat("x", 4097) + `","body_blocks":[],"seo":{}}`, `"0"`, 400},
			{"PUT", base + "/content/en", `{"title":"copy","body_blocks":[],"seo":[]}`, `"0"`, 400},
			{"GET", base + "/listings/33333333-3333-4333-8333-333333333333/en", "", "", 503},
			{"PUT", base + "/listings/33333333-3333-4333-8333-333333333333/en", `{"slug":"copy","listed":true,"content_revision":1}`, `"1"`, 503},
		} {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "b2fb4d26-ade5-5d77-9e64-03b2ece70eab")
			if tc.version != "" {
				req.Header.Set("If-Match", tc.version)
			}
			w := httptest.NewRecorder()
			server.Handler.ServeHTTP(w, req)
			want := tc.want
			if role == "viewer" || role == "content_editor" {
				want = 403
			}
			if w.Code != want {
				t.Errorf("CONTENT_ADMISSION %s %s %s HTTP%d want%d: %s", name, tc.method, tc.path, w.Code, want, w.Body.String())
			}
		}
	}
}
