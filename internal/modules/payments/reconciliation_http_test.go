// Responsibility: reject unauthorized/invalid reconciliation reads before storage.
// Dependencies: real generated routing; caller: foundation CI.
package payments

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type caseAuth struct{ deny bool }

func (a caseAuth) Authenticate(ctx context.Context, _ *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	if a.deny || spec.Scope != "returns:write" || spec.Access != httpapi.AdminOrAPIKey {
		return nil, apperrors.New(403, "forbidden", "Denied")
	}
	return ctx, nil
}

func TestRefundCaseReadAdmission(t *testing.T) {
	for _, tc := range []struct {
		query string
		deny  bool
		want  int
	}{
		{"", true, 403}, {"?after_id=bad", false, 400}, {"?unexpected=1", false, 400}, {"", false, 503},
	} {
		router := httpapi.New(caseAuth{deny: tc.deny}, nil)
		for _, route := range NewReconciliationHTTP(nil).Routes() {
			if err := router.Handle(route); err != nil {
				t.Fatal(err)
			}
		}
		r := httptest.NewRequest("GET", "/api/v1/t/11111111-1111-4111-8111-111111111111/payments/refund-cases"+tc.query, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("query=%s denied=%v: status=%d body=%s", tc.query, tc.deny, w.Code, w.Body.String())
		}
	}
}
