// Responsibilities: verify private admission snapshots and write-time callbacks.
// Dependencies: C-02c auth fixtures and neutral transaction queries; CI runs these
// regressions. Revoked authority must fail before business work, and caller input
// cannot mutate the tenant, subject, credential or scope captured by admission.
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

// Hiding the optional method proves that a legacy repository cannot silently
// authorize tenant writes merely because initial read admission succeeded.
type c02cReadAuthority struct {
	contract.AuthorizationRepository
}

func TestC02cTransactionAuthorizationCapturesAdmission(t *testing.T) {
	for _, key := range []bool{false, true} {
		f := c02cAuthFixture(t)
		f.handler.idle = 7 * time.Minute
		spec := c02cSpec(platform.AdminOrAPIKey, "POST", "catalog:write")
		q := f.request("POST", c02cTenant, !key, key)
		q.SetPathValue("tenant_id", c02cTenant)
		if !key {
			q.Header.Set("Origin", c02cOrigin)
			q.Header.Set("X-CSRF-Token", f.service.SessionCSRF(f.sessionToken))
		}
		ctx, err := f.handler.Authenticate(q.Context(), q, spec)
		if err != nil {
			t.Fatal(err)
		}
		view, ok := AuthorizationFromContext(ctx)
		if !ok || view.TenantID != c02cTenant || view.UserID != c02cUser {
			t.Fatal("missing admitted authorization snapshot")
		}
		for i := range view.Scopes {
			view.Scopes[i] = "attacker:write"
		}
		second, _ := AuthorizationFromContext(ctx)
		if len(second.Scopes) == 0 || second.Scopes[0] == "attacker:write" {
			t.Fatal("snapshot mutated admitted scope authority")
		}
		callback, err := f.handler.ResolveTransactionAuthorization(ctx, q, spec)
		if err != nil || callback == nil {
			t.Fatal("missing transaction authorizer", err)
		}
		q.SetPathValue("tenant_id", c02cOtherTenant)
		q.Header.Set("X-Actor-ID", c02cKeyID)
		q.Header.Set("X-Required-Scope", "attacker:write")
		queries := &merchantReceiptDB{}
		if err := callback(context.Background(), db.New(queries)); err != nil {
			t.Fatal(err)
		}
		if !key && queries.idle != 420 || key && queries.idle != 0 {
			t.Fatal("MDEL02_WRITE_CONFIGURED_IDLE", queries.idle)
		}
		kind, credential := contract.KindSession, c02cSession
		if key {
			kind, credential = contract.KindAPIKey, c02cKeyID
		}
		want := contract.TransactionAuthorization{UserID: c02cUser, Kind: kind, CredentialID: credential, Scope: "catalog:write"}
		if f.authority.revalidations != 1 || f.authority.revalidatedTenant != c02cTenant || f.authority.revalidatedGrant != want {
			t.Fatal("transaction authority trusted mutable request", f.authority.revalidatedGrant)
		}
		// Loss after request admission is still rejected when the transaction runs.
		f.authority.revalidationErr = apperrors.New(403, "scope_forbidden", "private DB detail")
		if err := callback(context.Background(), db.New(&merchantReceiptDB{})); err == nil || err.Error() == "private DB detail" {
			t.Fatal("transaction authority loss or private detail escaped", err)
		}
		if err := callback(context.Background(), nil); err == nil {
			t.Fatal("nil query transaction admitted")
		}
	}
}

func TestC02cTransactionAuthorizationFailsClosed(t *testing.T) {
	for _, name := range []string{"missing_port", "unadmitted_context", "other_route_scope", "other_tenant"} {
		t.Run(name, func(t *testing.T) {
			f := c02cAuthFixture(t)
			spec := c02cSpec(platform.AdminSession, "GET", "catalog:read")
			q := f.request("GET", c02cTenant, true, false)
			q.SetPathValue("tenant_id", c02cTenant)
			ctx, err := f.handler.Authenticate(q.Context(), q, spec)
			if err != nil {
				t.Fatal(err)
			}
			status := 401
			switch name {
			case "missing_port":
				f.handler.WithAuthorization(c02cReadAuthority{f.authority})
				status = 503
			case "unadmitted_context":
				ctx = context.Background()
			case "other_route_scope":
				spec.Scope = "catalog:write"
			case "other_tenant":
				q.SetPathValue("tenant_id", c02cOtherTenant)
			}
			callback, err := f.handler.ResolveTransactionAuthorization(ctx, q, spec)
			var public *apperrors.Error
			if callback != nil || !errors.As(err, &public) || public.Status != status {
				t.Fatal("untrusted or unsupported transaction admission", callback != nil, err)
			}
		})
	}
	if view, ok := AuthorizationFromContext(context.Background()); ok || view.UserID != "" {
		t.Fatal("unadmitted authorization snapshot")
	}
	f := c02cAuthFixture(t)
	callback, err := f.handler.ResolveTransactionAuthorization(context.Background(), f.request(http.MethodGet, c02cTenant, false, false), platform.RouteSpec{Access: platform.AuthenticatedSelf, Pattern: "/api/v1/auth/session"})
	if callback != nil || err != nil {
		t.Fatal("self route incorrectly needs tenant authorization")
	}
}

func TestC02cMissingAuthorizationRepositoryIsUnavailable(t *testing.T) {
	f := c02cAuthFixture(t)
	f.handler.WithAuthorization(nil)
	c02cAssert(t, f, f.serve(t, c02cSpec(platform.AdminSession, "GET", "catalog:read"), f.request("GET", c02cTenant, true, false)), 503, "", "", "")
}
