// Responsibilities: require write-transaction authorization to fail closed before handlers.
// Dependencies: Router's existing authentication seam and sqlc query port; called by CI.
// Invariant: adding a transaction authorization resolver cannot be silently ignored.
package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type transactionGuardAuth struct{ *projectedAuth }

func (a transactionGuardAuth) ResolveTransactionAuthorization(context.Context, *http.Request, httpapi.RouteSpec) (func(context.Context, *db.Queries) error, error) {
	return nil, apperrors.New(403, "forbidden", "Transaction authorization unavailable")
}
func TestC02cTransactionAuthorizationFailsClosed(t *testing.T) {
	a := transactionGuardAuth{&projectedAuth{actor: httpapi.Actor{Kind: "user", ID: actorUser}}}
	r := httpapi.New(a, nil)
	called := false
	spec := httpapi.RouteSpec{OperationID: "actorCommand", Method: "POST", Pattern: "/api/v1/t/{tenant_id}/carts", Access: httpapi.AdminSession, Scope: "catalog:write"}
	if err := r.Register(spec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(204) })); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/t/"+actorTenant+"/carts", nil))
	if w.Code != 403 || called {
		t.Fatalf("missing transaction authority reached handler: status %d called %v", w.Code, called)
	}
}
