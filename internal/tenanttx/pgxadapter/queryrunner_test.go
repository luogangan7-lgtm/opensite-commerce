// Responsibilities: prove the public central runner rejects a URL tenant without
// trusted routing. Dependencies: sqlc callback signature and platform stamp;
// Go gates call this test; no PostgreSQL or fabricated authenticated context.
package pgxadapter_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunAuthorizedRejectsUnstampedContext(t *testing.T) {
	var runner *pgxadapter.QueryRunner
	called := false
	for _, run := range []func(context.Context, string, func(*db.Queries) error) error{runner.RunAuthorized, runner.RunAuthorizedSnapshot} {
		err := run(context.Background(), "11111111-1111-1111-1111-111111111111", func(*db.Queries) error { called = true; return nil })
		if err == nil || called {
			t.Fatal("unstamped request reached database callback")
		}
	}
}

func TestRunAuthorizedTxRejectsUnstampedContext(t *testing.T) {
	var runner *pgxadapter.QueryRunner
	called := false
	err := runner.RunAuthorizedTx(context.Background(), "11111111-1111-1111-1111-111111111111", func(context.Context, pgx.Tx) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("unstamped command reached database callback")
	}
}

type transactionAuth struct{}

func (transactionAuth) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	return ctx, nil
}

func TestRunAuthorizedTxRejectsForeignTenant(t *testing.T) {
	var runner *pgxadapter.QueryRunner
	called := false
	router := httpapi.New(transactionAuth{}, nil)
	err := router.Register(httpapi.RouteSpec{OperationID: "authorizedTransactionProbe", Method: "POST", Pattern: "/api/v1/t/{tenant_id}/probe", Access: httpapi.AdminOrAPIKey, Scope: "test:write"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := runner.RunAuthorizedTx(r.Context(), "22222222-2222-4222-8222-222222222222", func(context.Context, pgx.Tx) error { called = true; return nil })
		var problem *apperrors.Error
		if !errors.As(err, &problem) || problem.Status != 404 {
			t.Errorf("foreign tenant not rejected: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatal(err)
	}
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/t/11111111-1111-4111-8111-111111111111/probe", nil))
	if called {
		t.Fatal("foreign tenant reached transaction callback")
	}
}
