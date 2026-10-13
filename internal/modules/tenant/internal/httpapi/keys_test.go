// Responsibilities: transport regression gates for key custody and strict input.
// Dependencies: real Router actor stamps and fake persistence, no database IO.
// Invariants: one-time secret matches only stored hash; no list secret or caching.
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant/contract"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

const testTenant = "11111111-1111-4111-8111-111111111111"
const testUser = "22222222-2222-4222-8222-222222222222"

type admitted struct{ kind string }

func (a admitted) Authenticate(ctx context.Context, _ *http.Request, _ platform.RouteSpec) (context.Context, error) {
	return ctx, nil
}
func (a admitted) ResolveActor(context.Context, *http.Request, platform.RouteSpec) (platform.Actor, error) {
	return platform.Actor{Kind: a.kind, ID: testUser}, nil
}

type repository struct {
	keys     []contract.APIKeyView
	material contract.KeyMaterial
	created  int
	revoked  int
}

func (r *repository) List(context.Context, string, string) ([]contract.APIKeyView, error) {
	return r.keys, nil
}
func (r *repository) Create(_ context.Context, tenant, user string, k contract.KeyMaterial) (contract.APIKeyView, error) {
	r.material = k
	r.created++
	v := contract.APIKeyView{ID: k.ID, KeyPrefix: k.Prefix, Scopes: k.Scopes, IssuedBy: user, CreatedAt: time.Now().UTC(), ExpiresAt: k.ExpiresAt}
	r.keys = append(r.keys, v)
	return v, nil
}
func (r *repository) Revoke(context.Context, string, string, string) error { r.revoked++; return nil }
func testRouter(t *testing.T, r contract.Repository, kind string) *platform.Router {
	t.Helper()
	router := platform.New(admitted{kind}, nil)
	for _, route := range New(r).Routes() {
		if err := router.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	return router
}
func call(router *platform.Router, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, request)
	return w
}
func TestAPIKeySecretOnlyAtCreate(t *testing.T) {
	repo := &repository{}
	router := testRouter(t, repo, "user")
	base := "/api/v1/t/" + testTenant + "/api-keys"
	w := call(router, "POST", base, `{"scopes":[]}`)
	if w.Code != 201 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("key creation/no-store", w.Code)
	}
	var created contract.Created
	if json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatal("created JSON")
	}
	pieces := strings.Split(created.Key, ".")
	if len(pieces) != 2 || pieces[0] != created.APIKey.ID {
		t.Fatal("public ID/secret format")
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(pieces[1])
	if e != nil || len(raw) != 32 || sha256.Sum256(raw) != repo.material.Hash {
		t.Fatal("stored hash does not match one-time secret")
	}
	if repo.material.Prefix != pieces[0][:8] {
		t.Fatal("prefix disclosed secret bytes")
	}
	listed := call(router, "GET", base, "")
	if listed.Code != 200 {
		t.Fatal("list refused")
	}
	if strings.Contains(listed.Body.String(), created.Key) || strings.Contains(listed.Body.String(), pieces[1]) || strings.Contains(listed.Body.String(), "key_hash") {
		t.Fatal("list disclosed credential material")
	}
	if strings.Contains(listed.Body.String(), `"expires_at"`) {
		t.Fatal("missing optional expiry serialized")
	}
	for range 2 {
		if revoked := call(router, "DELETE", base+"/"+created.APIKey.ID, ""); revoked.Code != 204 {
			t.Fatal("idempotent revoke", revoked.Code)
		}
	}
}
func TestAPIKeyInputAndActorAdmission(t *testing.T) {
	for _, body := range []string{`{}`, `{"scopes":null}`, `{"scopes":[],"expires_at":null}`, `{"scopes":["unknown:write"]}`, `{"scopes":["catalog:read","catalog:read"]}`, `{"scopes":[],"name":"unexpected"}`} {
		t.Run(body, func(t *testing.T) {
			repo := &repository{}
			w := call(testRouter(t, repo, "user"), "POST", "/api/v1/t/"+testTenant+"/api-keys", body)
			if w.Code != 400 || repo.created != 0 {
				t.Fatal("invalid input reached persistence", w.Code)
			}
		})
	}
	repo := &repository{}
	if w := call(testRouter(t, repo, "api_key"), "POST", "/api/v1/t/"+testTenant+"/api-keys", `{"scopes":[]}`); w.Code != 401 || repo.created != 0 {
		t.Fatal("key principal issued a key", w.Code)
	}
	if w := call(testRouter(t, nil, "user"), "GET", "/api/v1/t/"+testTenant+"/api-keys", ""); w.Code != 503 {
		t.Fatal("missing persistence did not fail closed", w.Code)
	}
}
