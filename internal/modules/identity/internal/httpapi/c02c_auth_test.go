// Responsibilities: production auth behavior witnesses through the real Router.
// Dependencies: the existing login service/repository double and platform context.
// Invariants: credential class, tenant, exact scope and CSRF precede handler work;
// actor IDs come from admission, never from cookies, URL IDs or spoofed headers.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

const (
	c02cTenant      = "10000000-0000-4000-8000-000000000001"
	c02cOtherTenant = "10000000-0000-4000-8000-000000000002"
	c02cUser        = "20000000-0000-4000-8000-000000000001"
	c02cSession     = "30000000-0000-4000-8000-000000000001"
	c02cKeyID       = "40000000-0000-4000-8000-000000000001"
	c02cOrigin      = "https://admin.example.test"
)

// These neutral fixture records precede the production contract deliberately:
// this tests-only commit compiles against main's existing constructors. After
// actual CI RED, fixture wiring may inject the authorization port, preserving
// every behavioral assertion below.
type c02cMembership struct{ UserID, TenantID, Role string }
type c02cKey struct {
	ID, TenantID, UserID string
	Hash                 [32]byte
	Scopes               []string
	Revoked, Expired     bool
}
type c02cAuthority struct {
	memberships                        map[string]c02cMembership
	keys                               map[string]c02cKey
	unavailable                        error
	membershipCalls, keyCalls, touches int
	revalidationErr                    error
	revalidations                      int
	revalidatedTenant                  string
	revalidatedGrant                   contract.TransactionAuthorization
}

func (a *c02cAuthority) Membership(_ context.Context, user, tenant string) (contract.Membership, error) {
	a.membershipCalls++
	if a.unavailable != nil {
		return contract.Membership{}, a.unavailable
	}
	m, ok := a.memberships[tenant]
	if !ok || m.UserID != user {
		return contract.Membership{}, contract.ErrNotFound
	}
	return contract.Membership{UserID: m.UserID, TenantID: m.TenantID, Role: m.Role}, nil
}
func (a *c02cAuthority) Key(_ context.Context, id string) (contract.APIKey, error) {
	a.keyCalls++
	if a.unavailable != nil {
		return contract.APIKey{}, a.unavailable
	}
	k, ok := a.keys[id]
	if !ok || k.Revoked || k.Expired {
		return contract.APIKey{}, contract.ErrNotFound
	}
	return contract.APIKey{ID: k.ID, TenantID: k.TenantID, UserID: k.UserID, Hash: k.Hash, Scopes: append([]string(nil), k.Scopes...)}, nil
}
func (a *c02cAuthority) TouchKey(_ context.Context, id string) error {
	if a.unavailable != nil {
		return a.unavailable
	}
	k, ok := a.keys[id]
	if !ok || k.Revoked || k.Expired {
		return contract.ErrNotFound
	}
	a.touches++
	return nil
}
func (a *c02cAuthority) Revalidate(_ context.Context, _ *db.Queries, tenant string, grant contract.TransactionAuthorization) error {
	a.revalidations++
	a.revalidatedTenant, a.revalidatedGrant = tenant, grant
	if a.unavailable != nil {
		return a.unavailable
	}
	return a.revalidationErr
}

type c02cFixture struct {
	handler              *Handler
	sessions             *fakeRepo
	service              *login.Service
	authority            *c02cAuthority
	sessionToken, bearer string
	calls                int
}

func c02cRandom(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}
func c02cAuthFixture(t *testing.T) *c02cFixture {
	t.Helper()
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	token := c02cRandom(t)
	repo := &fakeRepo{session: login.Session{DatabaseNow: now, ID: c02cSession, UserID: c02cUser, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}, hash: sha256.Sum256(token)}
	service, err := login.New(repo, login.Config{Origin: c02cOrigin, SigningKey: c02cRandom(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	secret := c02cRandom(t)
	authority := &c02cAuthority{
		memberships: map[string]c02cMembership{c02cTenant: {UserID: c02cUser, TenantID: c02cTenant, Role: "owner"}},
		keys:        map[string]c02cKey{c02cKeyID: {ID: c02cKeyID, TenantID: c02cTenant, UserID: c02cUser, Hash: sha256.Sum256(secret), Scopes: []string{"catalog:read", "catalog:write"}}},
	}
	return &c02cFixture{handler: New(service, c02cOrigin, 30*time.Minute).WithAuthorization(authority), sessions: repo, service: service, authority: authority, sessionToken: base64.RawURLEncoding.EncodeToString(token), bearer: c02cKeyID + "." + base64.RawURLEncoding.EncodeToString(secret)}
}
func c02cSpec(access platform.Access, method, scope string) platform.RouteSpec {
	return platform.RouteSpec{OperationID: "c02cAuthWitness", Method: method, Pattern: "/api/v1/t/{tenant_id}/products", Access: access, Scope: scope}
}

// Call the authenticator directly: the Router's own route validation must not
// hide a regression in the staff credential access-class allowlist.
func TestC02cStaffAuthenticatorRejectsOtherAccessClasses(t *testing.T) {
	for _, access := range []platform.Access{platform.Public, platform.Storefront, platform.InternalService, platform.AuthenticatedSelf, "unknown", ""} {
		for _, bearer := range []bool{false, true} {
			t.Run(string(access)+"/"+map[bool]string{false: "session", true: "api_key"}[bearer], func(t *testing.T) {
				f := c02cAuthFixture(t)
				r := f.request(http.MethodGet, c02cTenant, !bearer, bearer)
				r.SetPathValue("tenant_id", c02cTenant)
				ctx, err := f.handler.Authenticate(r.Context(), r, c02cSpec(access, http.MethodGet, "catalog:read"))
				w := httptest.NewRecorder()
				platform.WriteProblem(w, r, err)
				if err == nil || w.Code != http.StatusUnauthorized {
					t.Fatalf("non-staff access accepted valid staff credential: status=%d err=%v", w.Code, err)
				}
				if _, ok := AuthorizationFromContext(ctx); ok {
					t.Fatal("rejected access class received staff authority")
				}
				if f.sessions.lookups != 0 || f.authority.membershipCalls != 0 || f.authority.keyCalls != 0 || f.authority.touches != 0 {
					t.Fatal("rejected access class reached credential storage")
				}
			})
		}
	}
}
func (f *c02cFixture) request(method, tenant string, cookie, bearer bool) *http.Request {
	q := httptest.NewRequest(method, "/api/v1/t/"+tenant+"/products", nil)
	q.RemoteAddr = "192.0.2.8:12345"
	q.Header.Set("Idempotency-Key", "11111111-1111-4111-8111-111111111111")
	if cookie {
		q.AddCookie(&http.Cookie{Name: SessionCookie, Value: f.sessionToken})
	}
	if bearer {
		q.Header.Set("Authorization", "Bearer "+f.bearer)
	}
	return q
}
func (f *c02cFixture) serve(t *testing.T, spec platform.RouteSpec, q *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	router := platform.New(f.handler, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := router.Register(spec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		actor, err := platform.AuthorizedActor(r.Context())
		if err != nil {
			platform.WriteProblem(w, r, err)
			return
		}
		tenant := ""
		if strings.Contains(spec.Pattern, "{tenant_id}") {
			tenant, err = platform.AuthorizedTenant(r.Context())
			if err != nil {
				platform.WriteProblem(w, r, err)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(struct{ Kind, ID, Tenant string }{actor.Kind, actor.ID, tenant})
	})); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, q)
	return w
}
func c02cAssert(t *testing.T, f *c02cFixture, w *httptest.ResponseRecorder, status int, kind, id, tenant string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("auth status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
	if status != http.StatusOK {
		if f.calls != 0 {
			t.Fatalf("rejected request entered business handler %d times", f.calls)
		}
		return
	}
	var got struct{ Kind, ID, Tenant string }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != kind || got.ID != id || got.Tenant != tenant {
		t.Fatalf("actor/tenant projection=%+v want=%s/%s/%s", got, kind, id, tenant)
	}
	if f.calls != 1 {
		t.Fatalf("business handler count=%d", f.calls)
	}
}

func TestC02cAuthenticatedSelfProjectsStableUser(t *testing.T) {
	f := c02cAuthFixture(t)
	// No memberships: self-session admission and projection must remain usable.
	f.authority.memberships = map[string]c02cMembership{}
	spec := platform.RouteSpec{OperationID: "getAdminSession", Method: "GET", Pattern: "/api/v1/auth/session", Access: platform.AuthenticatedSelf}
	q := httptest.NewRequest("GET", spec.Pattern, nil)
	q.AddCookie(&http.Cookie{Name: SessionCookie, Value: f.sessionToken})
	q.Header.Set("X-Actor-ID", c02cOtherTenant)
	q.Header.Set("X-Actor-Kind", "api_key")
	c02cAssert(t, f, f.serve(t, spec, q), 200, "user", c02cUser, "")
	if f.sessions.lookups != 1 || f.authority.membershipCalls != 0 {
		t.Fatal("self admission repeated lookup or consulted tenant membership")
	}
}

func TestC02cSessionRoleScopes(t *testing.T) {
	for _, tc := range []struct {
		role, scope string
		status      int
	}{
		{"owner", "catalog:read", 200}, {"admin", "catalog:write", 200},
		{"catalog_editor", "catalog:read", 200}, {"catalog_editor", "catalog:write", 200}, {"catalog_editor", "cms:write", 403},
		{"content_editor", "catalog:read", 200}, {"content_editor", "cms:write", 200}, {"content_editor", "cms:publish", 200}, {"content_editor", "catalog:write", 403},
		{"viewer", "catalog:read", 200}, {"viewer", "catalog:write", 403},
		{"owner", "catalog:delete", 403}, {"admin", "catalog:*", 403}, {"unrecognized", "catalog:read", 403},
	} {
		t.Run(tc.role+"/"+tc.scope, func(t *testing.T) {
			for _, access := range []platform.Access{platform.AdminSession, platform.AdminOrAPIKey} {
				t.Run(string(access), func(t *testing.T) {
					f := c02cAuthFixture(t)
					f.authority.memberships[c02cTenant] = c02cMembership{UserID: c02cUser, TenantID: c02cTenant, Role: tc.role}
					q := f.request("GET", c02cTenant, true, false)
					q.Header.Set("X-Tenant-ID", c02cOtherTenant)
					q.Header.Set("X-Actor-ID", c02cKeyID)
					c02cAssert(t, f, f.serve(t, c02cSpec(access, "GET", tc.scope), q), tc.status, "user", c02cUser, c02cTenant)
				})
			}
		})
	}
}

// FIX-44: the orchestrator's exact grants apply to sessions and bearer keys;
// a stored key cannot expand a downgraded role or escape its issuing tenant.
func TestFIX44ScopeAdmission(t *testing.T) {
	for _, role := range []string{"owner", "admin", "catalog_editor", "content_editor", "viewer"} {
		for _, scope := range []string{"customers:privacy", "catalog:import", "catalog:export", "unknown:scope"} {
			for _, bearer := range []bool{false, true} {
				t.Run(role+"/"+scope+"/"+map[bool]string{false: "session", true: "key"}[bearer], func(t *testing.T) {
					f := c02cAuthFixture(t)
					f.authority.memberships[c02cTenant] = c02cMembership{UserID: c02cUser, TenantID: c02cTenant, Role: role}
					key := f.authority.keys[c02cKeyID]
					key.Scopes = []string{scope}
					f.authority.keys[c02cKeyID] = key
					status, kind, actor := 403, "user", c02cUser
					if scope != "unknown:scope" && (role == "owner" || role == "admin" || role == "catalog_editor" && scope != "customers:privacy") {
						status = 200
					}
					if bearer {
						kind, actor = "api_key", c02cKeyID
					}
					c02cAssert(t, f, f.serve(t, c02cSpec(platform.AdminOrAPIKey, "GET", scope), f.request("GET", c02cTenant, !bearer, bearer)), status, kind, actor, c02cTenant)
				})
			}
		}
	}
	for _, scope := range []string{"customers:privacy", "catalog:import", "catalog:export"} {
		t.Run("cross_tenant_key/"+scope, func(t *testing.T) {
			f := c02cAuthFixture(t)
			f.authority.memberships[c02cOtherTenant] = c02cMembership{UserID: c02cUser, TenantID: c02cOtherTenant, Role: "owner"}
			key := f.authority.keys[c02cKeyID]
			key.Scopes = []string{scope}
			f.authority.keys[c02cKeyID] = key
			c02cAssert(t, f, f.serve(t, c02cSpec(platform.AdminOrAPIKey, "GET", scope), f.request("GET", c02cOtherTenant, false, true)), 404, "", "", "")
		})
	}
}

func TestC02cSessionTenantMembershipAndDatabaseFailures(t *testing.T) {
	for _, name := range []string{"wrong_tenant", "suspended_membership", "suspended_tenant", "database_unavailable"} {
		t.Run(name, func(t *testing.T) {
			f := c02cAuthFixture(t)
			target, status := c02cTenant, 404
			switch name {
			case "wrong_tenant":
				target = c02cOtherTenant
			case "suspended_membership", "suspended_tenant":
				delete(f.authority.memberships, c02cTenant)
			case "database_unavailable":
				f.authority.unavailable = errors.New("private auth DSN and SQL")
				status = 503
			}
			w := f.serve(t, c02cSpec(platform.AdminSession, "GET", "catalog:read"), f.request("GET", target, true, false))
			c02cAssert(t, f, w, status, "", "", "")
			if strings.Contains(w.Body.String(), "private auth") {
				t.Fatal("database details escaped")
			}
		})
	}
}

func TestC02cMembershipLossIsObservedOnNextRequest(t *testing.T) {
	f := c02cAuthFixture(t)
	spec := c02cSpec(platform.AdminSession, "GET", "catalog:read")
	c02cAssert(t, f, f.serve(t, spec, f.request("GET", c02cTenant, true, false)), 200, "user", c02cUser, c02cTenant)
	delete(f.authority.memberships, c02cTenant)
	f.calls = 0
	c02cAssert(t, f, f.serve(t, spec, f.request("GET", c02cTenant, true, false)), 404, "", "", "")
	if f.sessions.lookups != 2 || f.authority.membershipCalls != 2 {
		t.Fatal("admission cached a session or tenant membership across requests")
	}
}

func TestC02cSessionWritesRequireBoundOriginAndCSRFBeforeLookup(t *testing.T) {
	for _, name := range []string{"valid", "missing_csrf", "foreign_origin", "duplicate_csrf", "duplicate_origin", "different_session_csrf"} {
		t.Run(name, func(t *testing.T) {
			f := c02cAuthFixture(t)
			q := f.request("POST", c02cTenant, true, false)
			q.Header.Set("Origin", c02cOrigin)
			q.Header.Set("X-CSRF-Token", f.service.SessionCSRF(f.sessionToken))
			switch name {
			case "missing_csrf":
				q.Header.Del("X-CSRF-Token")
			case "foreign_origin":
				q.Header.Set("Origin", "https://foreign.example.test")
			case "duplicate_csrf":
				q.Header.Add("X-CSRF-Token", q.Header.Get("X-CSRF-Token"))
			case "duplicate_origin":
				q.Header.Add("Origin", c02cOrigin)
			case "different_session_csrf":
				q.Header.Set("X-CSRF-Token", f.service.SessionCSRF(base64.RawURLEncoding.EncodeToString(c02cRandom(t))))
			}
			status := 403
			if name == "valid" {
				status = 200
			}
			c02cAssert(t, f, f.serve(t, c02cSpec(platform.AdminSession, "POST", "catalog:write"), q), status, "user", c02cUser, c02cTenant)
			if status == 403 && (f.sessions.lookups != 0 || f.authority.membershipCalls != 0) {
				t.Fatal("CSRF rejection refreshed session or queried membership")
			}
		})
	}
}

func TestC02cAPIKeyTenantBindingAndScopeIntersection(t *testing.T) {
	for _, access := range []platform.Access{platform.APIKey, platform.AdminOrAPIKey} {
		t.Run(string(access), func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				status int
			}{
				{"valid", 200}, {"wrong_secret", 401}, {"expired", 401}, {"revoked", 401},
				{"wrong_tenant_member_of_both", 404}, {"issuer_lost_membership", 401},
				{"key_scope_narrowed", 403}, {"issuer_role_narrowed", 403}, {"unknown_scope", 403}, {"database_unavailable", 503},
			} {
				t.Run(tc.name, func(t *testing.T) {
					f := c02cAuthFixture(t)
					target, scope := c02cTenant, "catalog:write"
					key := f.authority.keys[c02cKeyID]
					switch tc.name {
					case "wrong_secret":
						f.bearer = c02cKeyID + "." + base64.RawURLEncoding.EncodeToString(c02cRandom(t))
					case "expired":
						key.Expired = true
					case "revoked":
						key.Revoked = true
					case "wrong_tenant_member_of_both":
						target = c02cOtherTenant
						f.authority.memberships[c02cOtherTenant] = c02cMembership{UserID: c02cUser, TenantID: c02cOtherTenant, Role: "owner"}
					case "issuer_lost_membership":
						delete(f.authority.memberships, c02cTenant)
					case "key_scope_narrowed":
						key.Scopes = []string{"catalog:read"}
					case "issuer_role_narrowed":
						f.authority.memberships[c02cTenant] = c02cMembership{UserID: c02cUser, TenantID: c02cTenant, Role: "viewer"}
					case "unknown_scope":
						scope = "catalog:*"
						key.Scopes = []string{scope}
					case "database_unavailable":
						f.authority.unavailable = errors.New("private auth DSN and SQL")
					}
					f.authority.keys[c02cKeyID] = key
					q := f.request("POST", target, false, true)
					// Bearer requests do not need cookie CSRF, even for an unsafe method.
					q.Header.Set("Origin", "https://foreign.example.test")
					w := f.serve(t, c02cSpec(access, "POST", scope), q)
					c02cAssert(t, f, w, tc.status, "api_key", c02cKeyID, target)
					if strings.Contains(w.Body.String(), "private auth") {
						t.Fatal("database details escaped")
					}
					if tc.status != 200 && f.authority.touches != 0 {
						t.Fatal("failed key admission updated last-used timestamp")
					}
					if tc.status == 200 && f.authority.touches != 1 {
						t.Fatal("successful key admission did not touch exactly once")
					}
					if tc.name == "wrong_tenant_member_of_both" && f.authority.membershipCalls != 0 {
						t.Fatal("key tenant mismatch queried memberships before rejecting")
					}
					if f.sessions.lookups != 0 {
						t.Fatal("API key admission consulted admin session")
					}
				})
			}
		})
	}
}

func TestC02cCredentialClassesAndAmbiguityFailClosed(t *testing.T) {
	for _, name := range []string{"missing", "api_key_cookie_only", "admin_session_bearer_only", "mixed_credentials", "invalid_bearer_with_valid_cookie", "duplicate_cookie", "duplicate_authorization", "malformed_key"} {
		t.Run(name, func(t *testing.T) {
			f := c02cAuthFixture(t)
			access := platform.AdminOrAPIKey
			q := f.request("GET", c02cTenant, false, false)
			switch name {
			case "api_key_cookie_only":
				access = platform.APIKey
				q.AddCookie(&http.Cookie{Name: SessionCookie, Value: f.sessionToken})
			case "admin_session_bearer_only":
				access = platform.AdminSession
				q.Header.Set("Authorization", "Bearer "+f.bearer)
			case "mixed_credentials":
				q = f.request("GET", c02cTenant, true, true)
			case "invalid_bearer_with_valid_cookie":
				q = f.request("GET", c02cTenant, true, false)
				q.Header.Set("Authorization", "Bearer invalid")
			case "duplicate_cookie":
				q = f.request("GET", c02cTenant, true, false)
				q.AddCookie(&http.Cookie{Name: SessionCookie, Value: f.sessionToken})
			case "duplicate_authorization":
				q = f.request("GET", c02cTenant, false, true)
				q.Header.Add("Authorization", "Bearer "+f.bearer)
			case "malformed_key":
				q.Header.Set("Authorization", "Bearer "+c02cKeyID+".not-a-256-bit-secret")
			}
			c02cAssert(t, f, f.serve(t, c02cSpec(access, "GET", "catalog:read"), q), 401, "", "", "")
			if f.sessions.lookups != 0 || f.authority.keyCalls != 0 || f.authority.membershipCalls != 0 {
				t.Fatal("malformed or ambiguous credentials reached auth database")
			}
		})
	}
}

func TestC02cActorResolverCannotTrustUnadmittedInput(t *testing.T) {
	f := c02cAuthFixture(t)
	resolver, ok := any(f.handler).(platform.ActorResolver)
	if !ok {
		t.Fatal("production authenticator has no ActorResolver")
	}
	q := f.request("GET", c02cTenant, true, true)
	q.Header.Set("X-Actor-ID", c02cUser)
	actor, err := resolver.ResolveActor(context.Background(), q, c02cSpec(platform.AdminOrAPIKey, "GET", "catalog:read"))
	if err == nil || actor != (platform.Actor{}) {
		t.Fatal("unadmitted input produced a stable actor")
	}
}
