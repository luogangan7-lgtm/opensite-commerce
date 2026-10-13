//go:build pgx

// Responsibilities: prove the checked transaction seam refuses untrusted and
// mismatched tenants before touching the pool. Dependencies: central HTTP stamp,
// pgx transaction callback; focused security gates; no raw connections or pool.
package pgxadapter_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type authorizeTxProbe struct{}

func (authorizeTxProbe) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	return ctx, nil
}

func stampedTxContext(t *testing.T, access httpapi.Access) context.Context {
	t.Helper()
	var ctx context.Context
	router := httpapi.New(authorizeTxProbe{}, nil)
	spec := httpapi.RouteSpec{OperationID: "txProbe", Method: "GET", Pattern: "/t/{tenant_id}", Access: access}
	if access != httpapi.Public {
		spec.Scope = "checkout.write"
	}
	if err := router.Register(spec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx = r.Context()
		w.WriteHeader(http.StatusNoContent)
	})); err != nil {
		t.Fatal(err)
	}
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/t/11111111-1111-1111-1111-111111111111", nil))
	if ctx == nil {
		t.Fatal("probe did not reach handler")
	}
	return ctx
}

func TestRunAuthorizedTxRejectsBeforePool(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		tenant string
		status int
		code   string
	}{
		{"unstamped", context.Background(), "11111111-1111-1111-1111-111111111111", 401, "unauthenticated"},
		{"nil context", nil, "11111111-1111-1111-1111-111111111111", 401, "unauthenticated"},
		{"public route", stampedTxContext(t, httpapi.Public), "11111111-1111-1111-1111-111111111111", 401, "unauthenticated"},
		{"tenant mismatch", stampedTxContext(t, httpapi.InternalService), "22222222-2222-2222-2222-222222222222", 404, "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var runner pgxadapter.Runner
			called := false
			err := runner.RunAuthorizedTx(tc.ctx, tc.tenant, func(context.Context, pgx.Tx) error { called = true; return nil })
			var problem *apperrors.Error
			if !errors.As(err, &problem) || problem.Status != tc.status || problem.Code != tc.code || called {
				t.Fatalf("error=%v callback=%v want %d/%s without callback", err, called, tc.status, tc.code)
			}
		})
	}
}

func TestRunAuthorizedTxRejectsUnavailableAndNilCallback(t *testing.T) {
	ctx := stampedTxContext(t, httpapi.InternalService)
	const tenant = "11111111-1111-1111-1111-111111111111"
	var runner pgxadapter.Runner
	called := false
	err := runner.RunAuthorizedTx(ctx, tenant, func(context.Context, pgx.Tx) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("unavailable pool reached callback: %v called=%v", err, called)
	}
	if err = runner.RunAuthorizedTx(ctx, tenant, nil); err == nil || err.Error() != "transaction callback is nil" {
		t.Fatalf("nil callback: %v", err)
	}
}
