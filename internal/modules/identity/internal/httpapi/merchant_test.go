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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
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
