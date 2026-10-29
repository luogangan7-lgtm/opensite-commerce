// Responsibility: exercise dormant merchant transport through the actual Router.
// Dependencies: existing session fixture, real login admission/KDF, digest fake.
// Invariants: CSRF/role/credential failures precede proof writes; no rotation;
// terminal receipt authority cannot leak into any other route or write.
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"golang.org/x/crypto/argon2"
)

type merchantRepo struct {
	*fakeRepo
	issued         int
	digest         [32]byte
	actor          login.Actor
	tenant, action string
	receipt        bool
}

func (r *merchantRepo) MerchantIdentifier(context.Context, string) (string, error) {
	return "owner@invalid.test", nil
}
func (r *merchantRepo) IssueMerchantProof(_ context.Context, tenant string, actor login.Actor, action string, digest [32]byte, hash string) (time.Time, error) {
	if hash != r.credential.PasswordHash {
		return time.Time{}, login.ErrCredentials
	}
	r.issued++
	r.digest = digest
	r.actor = actor
	r.tenant = tenant
	r.action = action
	return time.Date(2030, 1, 1, 0, 5, 0, 0, time.UTC), nil
}
func (r *merchantRepo) MerchantReceipt(context.Context, string, string, login.Actor) (bool, error) {
	return r.receipt, nil
}

func merchantFixture(t *testing.T) (*c02cFixture, *merchantRepo) {
	t.Helper()
	f := c02cAuthFixture(t)
	salt := []byte("0123456789abcdef")
	hash := argon2.IDKey([]byte("correct-password"), salt, 2, 19456, 1, 32)
	f.sessions.credential = login.Credential{UserID: c02cUser, Status: "active", PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash)}
	repo := &merchantRepo{fakeRepo: f.sessions}
	service, err := login.New(repo, login.Config{Origin: c02cOrigin, SigningKey: c02cRandom(t), Now: func() time.Time { return f.sessions.session.DatabaseNow }})
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	f.handler = New(service, c02cOrigin, 30*time.Minute).WithAuthorization(f.authority)
	return f, repo
}

func TestMerchantStepUpTransport(t *testing.T) {
	for _, name := range []string{"owner", "admin", "key", "customer", "csrf", "origin", "stale", "cross-tenant", "password", "cooldown", "action", "empty-password"} {
		t.Run(name, func(t *testing.T) {
			f, repo := merchantFixture(t)
			r := httptest.NewRequest("POST", "/api/v1/t/"+c02cTenant+"/merchant-deletion/step-up", strings.NewReader(`{"action":"request","password":"correct-password"}`))
			r.RemoteAddr = "192.0.2.8:12345"
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", c02cOrigin)
			r.Header.Set("X-CSRF-Token", f.service.SessionCSRF(f.sessionToken))
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: f.sessionToken})
			expected := 200
			switch name {
			case "admin":
				f.authority.memberships[c02cTenant] = c02cMembership{UserID: c02cUser, TenantID: c02cTenant, Role: "admin"}
				expected = 403
			case "key":
				r.Header.Del("Cookie")
				r.Header.Set("Authorization", "Bearer "+f.bearer)
				expected = 401
			case "customer":
				r.Header.Del("Cookie")
				r.AddCookie(&http.Cookie{Name: "customer_session", Value: f.sessionToken})
				expected = 401
			case "csrf":
				r.Header.Del("X-CSRF-Token")
				expected = 403
			case "origin":
				r.Header.Set("Origin", "https://attacker.invalid")
				expected = 403
			case "stale":
				f.sessions.dead = true
				expected = 401
			case "cross-tenant":
				r.URL.Path = strings.Replace(r.URL.Path, c02cTenant, c02cOtherTenant, 1)
				expected = 404
			case "password":
				r.Body = io.NopCloser(strings.NewReader(`{"action":"request","password":"wrong-password"}`))
				expected = 401
			case "cooldown":
				f.sessions.accountCooldown = time.Now().Add(time.Hour)
				expected = 429
			case "action":
				r.Body = io.NopCloser(strings.NewReader(`{"action":"execute","password":"correct-password"}`))
				expected = 422
			case "empty-password":
				r.Body = io.NopCloser(strings.NewReader(`{"action":"request","password":""}`))
				expected = 422
			}
			router := platform.New(f.handler, slog.New(slog.NewTextHandler(io.Discard, nil)))
			route := f.handler.MerchantStepUpRoute()
			if err := router.Register(route.Spec, route.Handler); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("MDEL02_HTTP_CACHE_CONTROL")
			}
			if w.Code != expected {
				t.Fatalf("MDEL02_HTTP_%s status=%d body=%s", name, w.Code, w.Body.String())
			}
			if f.sessions.saved != 0 || f.sessions.revoked != 0 {
				t.Fatal("MDEL02_STEPUP_ROTATED_GLOBAL_SESSION")
			}
			if name == "owner" {
				var proof login.MerchantProof
				if err := json.Unmarshal(w.Body.Bytes(), &proof); err != nil {
					t.Fatal(err)
				}
				raw, err := base64.RawURLEncoding.DecodeString(proof.Proof)
				if err != nil || len(raw) != 32 || sha256.Sum256(raw) != repo.digest || repo.issued != 1 || repo.tenant != c02cTenant || repo.actor.UserID() != c02cUser || repo.actor.SessionID() != c02cSession || repo.action != "request" || proof.ExpiresAt.Year() != 2030 {
					t.Fatal("MDEL02_HTTP_PROOF_BINDING")
				}
			} else if repo.issued != 0 {
				t.Fatal("MDEL02_DENIED_PROOF_WRITE")
			}
			if name == "password" && repo.failures != 1 {
				t.Fatal("MDEL02_STEPUP_FAILURE_BOOKKEEPING")
			}
		})
	}
}

func TestMerchantTerminalReceiptIsExactRouteOnly(t *testing.T) {
	for _, name := range []string{"receipt", "business", "write", "wrong-operation"} {
		t.Run(name, func(t *testing.T) {
			f, repo := merchantFixture(t)
			f.handler.idle = 7 * time.Minute
			repo.receipt = true
			delete(f.authority.memberships, c02cTenant)
			spec := platform.RouteSpec{OperationID: "getMerchantDeletion", Method: "GET", Pattern: "/api/v1/t/{tenant_id}/merchant-deletion/{request_id}", Access: platform.AdminSession, Scope: "tenant:delete"}
			if name == "business" {
				spec = c02cSpec(platform.AdminSession, "GET", "catalog:read")
			}
			if name == "write" {
				spec.Method = "POST"
			}
			if name == "wrong-operation" {
				spec.OperationID = "getAnotherReceipt"
			}
			r := f.request(spec.Method, c02cTenant, true, false)
			r.SetPathValue("tenant_id", c02cTenant)
			r.SetPathValue("request_id", c02cKeyID)
			r.Header.Set("Origin", c02cOrigin)
			r.Header.Set("X-CSRF-Token", f.service.SessionCSRF(f.sessionToken))
			ctx, err := f.handler.Authenticate(r.Context(), r, spec)
			if name == "receipt" {
				if err != nil {
					t.Fatal(err)
				}
				view, ok := AuthorizationFromContext(ctx)
				if !ok || !view.ReceiptOnly {
					t.Fatal("MDEL02_RECEIPT_NOT_RESTRICTED")
				}
			} else if err == nil {
				t.Fatal("MDEL02_RECEIPT_SCOPE_ESCAPED")
			}
		})
	}
}

func TestMerchantRoutesRemainDormant(t *testing.T) {
	f := c02cAuthFixture(t)
	for _, route := range f.handler.Routes() {
		if strings.Contains(route.Spec.Pattern, "merchant-deletion") {
			t.Fatal("MDEL02_PREMATURE_ROUTE_ACTIVATION")
		}
	}
}

// A live admitted subject losing tenant write authority receives Forbidden.
func TestMerchantCoolingWriteIsForbidden(t *testing.T) {
	for _, state := range []string{"cooling_off", "failed", "active-role-loss"} {
		t.Run(state, func(t *testing.T) {
			f := c02cAuthFixture(t)
			spec := c02cSpec(platform.AdminSession, "POST", "catalog:write")
			r := f.request("POST", c02cTenant, true, false)
			r.SetPathValue("tenant_id", c02cTenant)
			r.Header.Set("Origin", c02cOrigin)
			r.Header.Set("X-CSRF-Token", f.service.SessionCSRF(f.sessionToken))
			ctx, err := f.handler.Authenticate(r.Context(), r, spec)
			if err != nil {
				t.Fatal(err)
			}
			check, err := f.handler.ResolveTransactionAuthorization(ctx, r, spec)
			if err != nil || check == nil {
				t.Fatal("missing transaction check", err)
			}
			f.authority.revalidationErr = contract.ErrNotFound
			q := &merchantReceiptDB{allowed: state == "active-role-loss"}
			expected := 403
			if state == "active-role-loss" {
				expected = 401
			}
			var public *apperrors.Error
			if err := check(ctx, db.New(q)); !errors.As(err, &public) || public.Status != expected {
				t.Fatalf("MDEL02_COOLING_HTTP_FORBIDDEN: %v", err)
			}
		})
	}
}

type merchantReceiptDB struct {
	db.DBTX
	allowed bool
	calls   int
	args    []any
	idle    int64
}

func (q *merchantReceiptDB) Exec(_ context.Context, sql string, args ...any) (pgxadapter.CommandTag, error) {
	if strings.Contains(sql, "LoginSetTransactionIdle") {
		q.idle = args[0].(int64)
	}
	return pgxadapter.CommandTag{}, nil
}

func (q *merchantReceiptDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	q.calls++
	q.args = args
	return q
}
func (q *merchantReceiptDB) Scan(dest ...any) error {
	*dest[0].(*bool) = q.allowed
	return nil
}

func TestMerchantReceiptRegisteredReadRevalidates(t *testing.T) {
	for _, name := range []string{"requester", "current-owner", "revoked-after-admission", "other-request", "other-tenant", "admin", "key", "customer", "stale", "invalid-request"} {
		t.Run(name, func(t *testing.T) {
			f, repo := merchantFixture(t)
			f.handler.idle = 7 * time.Minute
			repo.receipt = true
			delete(f.authority.memberships, c02cTenant)
			q := &merchantReceiptDB{allowed: true}
			if name == "current-owner" {
				f.authority.memberships[c02cTenant] = c02cMembership{UserID: c02cUser, TenantID: c02cTenant, Role: "owner"}
			}
			if name == "revoked-after-admission" {
				q.allowed = false
			}
			if name == "other-request" || name == "other-tenant" {
				repo.receipt = false
			}
			if name == "admin" {
				f.authority.memberships[c02cTenant] = c02cMembership{UserID: c02cUser, TenantID: c02cTenant, Role: "admin"}
			}
			if name == "stale" {
				f.sessions.dead = true
			}
			called := false
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := platform.RevalidateTransaction(r.Context(), db.New(q)); err != nil {
					platform.WriteProblem(w, r, err)
					return
				}
				called = true
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"request_id":"`+c02cKeyID+`","state":"deleted","phase":"completed","epoch":2,"requested_at":"2030-01-01T00:00:00Z","execute_after":"2030-01-31T00:00:00Z","attempt":1,"next_retry_at":null,"categories":{}}`)
			})
			port, ok := any(f.handler).(interface {
				MerchantReceiptRoute(http.Handler) platform.Route
			})
			if !ok {
				t.Fatal("MDEL02_RECEIPT_ROUTE_PORT_REQUIRED")
			}
			route := port.MerchantReceiptRoute(handler)
			router := platform.New(f.handler, nil)
			if err := router.Register(route.Spec, route.Handler); err != nil {
				t.Fatal("MDEL02_RECEIPT_REGISTER", err)
			}
			r := httptest.NewRequest("GET", "/api/v1/t/"+c02cTenant+"/merchant-deletion/"+c02cKeyID, nil)
			if name == "other-tenant" {
				r.URL.Path = strings.Replace(r.URL.Path, c02cTenant, c02cOtherTenant, 1)
			}
			if name == "invalid-request" {
				r.URL.Path = strings.Replace(r.URL.Path, c02cKeyID, "invalid", 1)
			}
			r.RemoteAddr = "192.0.2.8:12345"
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: f.sessionToken})
			if name == "key" {
				r.Header.Del("Cookie")
				r.Header.Set("Authorization", "Bearer "+f.bearer)
			}
			if name == "customer" {
				r.Header.Del("Cookie")
				r.AddCookie(&http.Cookie{Name: "customer_session", Value: f.sessionToken})
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			expected := 200
			switch name {
			case "revoked-after-admission", "admin":
				expected = 403
			case "other-request", "other-tenant":
				expected = 404
			case "key", "customer", "stale":
				expected = 401
			case "invalid-request":
				expected = 400
			}
			if w.Code != expected || called != (expected == 200) || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("MDEL02_RECEIPT_READ: status=%d called=%v body=%s", w.Code, called, w.Body.String())
			}
			if (name == "requester" || name == "current-owner") && (q.calls != 1 || len(q.args) != 4 || q.idle != 420) {
				t.Fatal("MDEL02_RECEIPT_READ_NOT_REVALIDATED", q.calls)
			}
			if called {
				for i, expected := range []string{c02cTenant, c02cKeyID, c02cUser, c02cSession} {
					if q.args[i].(pgtype.UUID).String() != expected {
						t.Fatal("MDEL02_RECEIPT_READ_BINDING", i)
					}
				}
			}
		})
	}
}

// The guard must live with Tenant; Identity may only call its public fence.
func TestMerchantTenantGuardOwnership(t *testing.T) {
	files, err := filepath.Glob("../../../../../migrations/*_identity_merchant_deletion.sql")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := filepath.Glob("../../../../../migrations/pending/*_identity_merchant_deletion.sql")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, pending...)
	if len(files) != 1 {
		t.Fatal("MDEL02_IDENTITY_MIGRATION_WITNESS", files)
	}
	source, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), "p.proname='tenant_lifecycle_transition'") {
		t.Fatal("MDEL02_TENANT_GUARD_OWNERSHIP: Identity rewrites Tenant private transition")
	}
}
