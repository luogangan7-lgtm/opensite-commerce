// Responsibility: admission, validation and error mapping of the admin order
// read API through the production platform router, without a database.
// Dependencies: platform httpapi router, generated DTOs; caller: go test gates.
// Invariant: rejected requests never reach the snapshot runner.
package orders

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
)

const tenantA = "11111111-1111-4111-8111-111111111111"
const tenantB = "22222222-2222-4222-8222-222222222222"
const orderID = "99999999-9999-4999-8999-999999999999"

type testAuth struct {
	tenant string
	denied bool
}

func (a testAuth) Authenticate(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	if a.denied || spec.Access != httpapi.AdminOrAPIKey || spec.Scope != "orders:read" {
		return nil, apperrors.New(403, "forbidden", "Scope denied")
	}
	if r.PathValue("tenant_id") != a.tenant {
		return nil, apperrors.New(404, "not_found", "Tenant not found")
	}
	return ctx, nil
}

func TestOrdersHTTPAdmission(t *testing.T) {
	list := "/api/v1/t/" + tenantA + "/orders"
	valid := encodeOrderCursor(orderCursor{Time: time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC), ID: uuid.MustParse(orderID)})
	paid := encodeOrderCursor(orderCursor{Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), ID: uuid.MustParse(orderID), State: "paid"})
	raw := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	storage := errors.New("dial tcp 10.0.0.1: secret detail")
	for _, tc := range []struct {
		name, method, path string
		auth               httpapi.Authenticator
		runErr             error
		noRunner           bool
		want, calls        int
	}{
		{name: "default deny", method: "GET", path: list, want: 401},
		{name: "scope denied", method: "GET", path: list, auth: testAuth{tenant: tenantA, denied: true}, want: 403},
		{name: "foreign tenant", method: "GET", path: "/api/v1/t/" + tenantB + "/orders/" + orderID, auth: testAuth{tenant: tenantA}, want: 404},
		{name: "write method not routed", method: "POST", path: list, auth: testAuth{tenant: tenantA}, want: 404},
		{name: "delete not routed", method: "DELETE", path: list + "/" + orderID, auth: testAuth{tenant: tenantA}, want: 404},
		{name: "non-uuid order", method: "GET", path: list + "/not-a-uuid", auth: testAuth{tenant: tenantA}, want: 400},
		{name: "limit zero", method: "GET", path: list + "?limit=0", auth: testAuth{tenant: tenantA}, want: 400},
		{name: "limit too large", method: "GET", path: list + "?limit=101", auth: testAuth{tenant: tenantA}, want: 400},
		{name: "limit not integer", method: "GET", path: list + "?limit=abc", auth: testAuth{tenant: tenantA}, want: 400},
		{name: "unknown state", method: "GET", path: list + "?state=shipped", auth: testAuth{tenant: tenantA}, want: 400},
		{name: "cursor garbage", method: "GET", path: list + "?cursor=!!", auth: testAuth{tenant: tenantA}, want: 400},
		{name: "cursor empty", method: "GET", path: list + "?cursor=", auth: testAuth{tenant: tenantA}, want: 400},
		{name: "cursor padded", method: "GET", path: list + "?cursor=" + valid + "%3D", auth: testAuth{tenant: tenantA}, want: 400},
		{name: "cursor extra field", method: "GET", path: list + "?cursor=" + raw(`{"t":"2026-01-02T03:04:05Z","id":"`+orderID+`","s":"","x":1}`), auth: testAuth{tenant: tenantA}, want: 400},
		{name: "cursor non-canonical", method: "GET", path: list + "?cursor=" + raw(`{"id":"`+orderID+`","t":"2026-01-02T03:04:05Z","s":""}`), auth: testAuth{tenant: tenantA}, want: 400},
		{name: "cursor nil id", method: "GET", path: list + "?cursor=" + raw(`{"t":"2026-01-02T03:04:05Z","id":"00000000-0000-0000-0000-000000000000","s":""}`), auth: testAuth{tenant: tenantA}, want: 400},
		{name: "cursor bound to other filter", method: "GET", path: list + "?cursor=" + paid, auth: testAuth{tenant: tenantA}, want: 400},
		{name: "cursor filter mismatch", method: "GET", path: list + "?state=cancelled&cursor=" + paid, auth: testAuth{tenant: tenantA}, want: 400},
		{name: "storage error is sanitized 503", method: "GET", path: list + "?state=paid&cursor=" + paid, auth: testAuth{tenant: tenantA}, runErr: storage, want: 503, calls: 1},
		{name: "valid cursor reaches runner", method: "GET", path: list + "?limit=100&cursor=" + valid, auth: testAuth{tenant: tenantA}, runErr: storage, want: 503, calls: 1},
		{name: "unconfigured persistence is 503", method: "GET", path: list, auth: testAuth{tenant: tenantA}, noRunner: true, want: 503},
		{name: "missing order is 404", method: "GET", path: list + "/" + orderID, auth: testAuth{tenant: tenantA}, runErr: pgx.ErrNoRows, want: 404, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			h := NewHTTP(HTTPConfig{Read: func(context.Context, string, func(*db.Queries) error) error {
				calls++
				return tc.runErr
			}})
			if tc.noRunner {
				h = NewHTTP(HTTPConfig{})
			}
			r := httpapi.New(tc.auth, nil)
			for _, route := range h.Routes() {
				if err := r.Register(route.Spec, route.Handler); err != nil {
					t.Fatal(err)
				}
			}
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
			if rr.Code != tc.want || calls != tc.calls {
				t.Fatalf("HTTP %d want %d, calls %d want %d: %s", rr.Code, tc.want, calls, tc.calls, rr.Body.String())
			}
			var problem map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil || problem["status"] != float64(tc.want) ||
				!strings.HasPrefix(rr.Header().Get("Content-Type"), "application/problem+json") || strings.Contains(rr.Body.String(), "secret") {
				t.Fatalf("problem mapping: %s", rr.Body.String())
			}
		})
	}
}

func TestOrdersDirectCallRejectsUntrustedContext(t *testing.T) {
	calls := 0
	h := NewHTTP(HTTPConfig{Read: func(context.Context, string, func(*db.Queries) error) error { calls++; return nil }})
	_, err := h.GetOrder(context.Background(), generated.GetOrderRequestObject{TenantId: uuid.MustParse(tenantA), OrderId: uuid.MustParse(orderID)})
	var domain *apperrors.Error
	if !errors.As(err, &domain) || domain.Status != 401 || calls != 0 {
		t.Fatalf("direct request: %v calls %d", err, calls)
	}
}

func TestOrderCursorRoundTrip(t *testing.T) {
	c := orderCursor{Time: time.Date(2026, 5, 6, 7, 8, 9, 999999000, time.FixedZone("x", 3600)), ID: uuid.MustParse(orderID), State: "refunded"}
	encoded := encodeOrderCursor(c)
	got, has, err := parseOrderCursor(&encoded, "refunded")
	if err != nil || !has || !got.Time.Equal(c.Time) || got.ID != c.ID {
		t.Fatalf("round trip: %+v %v %v", got, has, err)
	}
	if _, _, err = parseOrderCursor(&encoded, ""); err == nil {
		t.Fatal("cursor accepted under a different filter")
	}
}
