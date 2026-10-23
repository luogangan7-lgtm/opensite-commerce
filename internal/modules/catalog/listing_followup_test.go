// Responsibility: pin operation-specific catalog error mapping at the HTTP boundary.
// Dependencies: real catalog router and injected transaction errors; caller: CI.
// Invariant: listing FK/slug failures never change other catalog operations.
package catalog

import (
	"context"
	"strings"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestListingErrorScope(t *testing.T) {
	base := "/api/v1/t/" + tenantA + "/products/" + missingID
	for _, tc := range []struct {
		name, path, code, body, want string
		status                       int
	}{
		{"content FK stays generic", base + "/content/en", "23503", `{"title":"ok","body_blocks":[],"seo":{}}`, "internal_error", 500},
		{"listing FK", base + "/listings/" + missingID + "/en", "23503", `{"slug":"taken","listed":true,"content_revision":1}`, "Listing site or content revision", 422},
		{"listing slug", base + "/listings/" + missingID + "/en", "23505", `{"slug":"taken","listed":true,"content_revision":1}`, "listing_slug_conflict", 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Run: func(context.Context, string, func(*db.Queries) error) error {
				return &pgxadapter.PgError{Code: tc.code}
			}})
			w := httpRequest(testRouter(t, s, testAuth{tenant: tenantA}), "PUT", tc.path, tc.body, `"1"`)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("ERROR_SCOPE %d %s", w.Code, w.Body.String())
			}
		})
	}
}
