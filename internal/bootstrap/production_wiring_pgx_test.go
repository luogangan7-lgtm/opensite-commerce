//go:build pgx

// Responsibility: reconcile every implemented operation with production admission
// and persistence composition, not a test-installed route or trusted actor.
// Dependencies: FromEnvironment/New, existing disposable PG and credential fixtures.
// Foundation's pgx lane runs this gate; missing bindings and stale exceptions fail.
package bootstrap_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func TestProductionWiringPG(t *testing.T) {
	f := quoteOrdersDB(t, time.Hour)
	customer, secret, customerCookie := quoteCustomer(t, f)
	host := "wiring-" + f.site[:8] + ".example.test"
	origin := "https://" + host
	raw := c02cHTTPRandom(t, 32)
	staffToken := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256(raw)
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES($1,$2,clock_timestamp()+interval '1 hour')`, f.staff, hash[:])
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, f.tenant); err != nil {
			return err
		}
		release := uuid.NewString()
		if _, err = tx.Exec(ctx, `INSERT INTO app.site_releases(tenant_id,site_id,id,release_no,renderer_build_id,manifest,manifest_hash) VALUES($1,$2,$3,1,'wiring','{"version":1,"locale":"en","title":"Wiring","description":"","navigation":[],"products":[]}',sha256(convert_to('wiring','UTF8')))`, f.tenant, f.site, release); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `SET LOCAL ROLE app_domain_verifier`); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at) VALUES($1,$2,$3,'verified',clock_timestamp())`, f.tenant, f.site, host); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `RESET ROLE; SET LOCAL ROLE app_publisher`); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `SELECT app.publish_release($1,$2,$3,NULL)`, f.tenant, f.site, release)
		return err
	})
	publicDSN := loginDSN(t, f.ctx, f.runner.Runner, os.Getenv("DATABASE_URL"), "wiring_public_", "app_domain_resolver")
	metadata, err := json.Marshal(checkout.RuleMetadata{Profiles: map[string]string{f.tenant: f.profile}, SKUs: map[string]map[string]checkout.SKUMetadata{f.tenant: {f.sku: {TaxCategory: "standard", WeightG: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"OPENSITE_DATABASE_URL":         loginDSN(t, f.ctx, f.runner.Runner, os.Getenv("DATABASE_URL"), "wiring_runtime_", "app_runtime"),
		"OPENSITE_AUTH_DATABASE_URL":    loginDSN(t, f.ctx, f.runner.Runner, os.Getenv("DATABASE_URL"), "wiring_auth_", "app_auth"),
		"OPENSITE_MEDIA_DATABASE_URL":   loginDSN(t, f.ctx, f.runner.Runner, os.Getenv("DATABASE_URL"), "wiring_media_", "app_media_service"),
		"OPENSITE_PRIVACY_DATABASE_URL": loginDSN(t, f.ctx, f.runner.Runner, os.Getenv("DATABASE_URL"), "wiring_privacy_", "app_runtime"),
		"OPENSITE_MEDIA_ROOT":           t.TempDir(), "OPENSITE_IDEMPOTENCY_ROOT": t.TempDir(),
		"OSC_PUBLIC_DATABASE_URL": publicDSN, "OSC_ADMIN_ORIGIN": origin,
		"OSC_AUTH_SIGNING_KEY_HEX":      hex.EncodeToString(quoteRandom(t)),
		"OPENSITE_CMS_PREVIEW_KEY":      base64.RawURLEncoding.EncodeToString(quoteRandom(t)),
		"PRIVACY_TOKEN_KEY":             hex.EncodeToString(quoteRandom(t)),
		"PRIVACY_EXPORT_WORKER_ENABLED": "true", "JOB_SIGNING_KEY": hex.EncodeToString(quoteRandom(t)),
		"OPENSITE_ENVIRONMENT": "test", "OPENSITE_FAKE_PSP": "1",
		"OPENSITE_FAKE_WEBHOOK_KEY":     base64.RawURLEncoding.EncodeToString(quoteRandom(t)),
		"OPENSITE_STOREFRONT_KEY":       base64.RawURLEncoding.EncodeToString(quoteRandom(t)),
		"OPENSITE_STOREFRONT_TENANT_ID": f.tenant, "OPENSITE_STOREFRONT_ORIGIN": origin,
		"OPENSITE_CHECKOUT_RULE_METADATA": string(metadata),
	} {
		t.Setenv(name, value)
	}
	// Same public handler and dedicated role as cmd/api/storefront_pgx.go.
	public, closePublic, err := publish.Open(f.ctx, publicDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closePublic)
	cfg, closeRuntime, err := bootstrap.FromEnvironment(f.ctx, bootstrap.Config{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Customer: customer, CustomerOrigin: origin, CustomerSecret: secret,
		Routes: []httpapi.Route{publish.StorefrontRoute(public)},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeRuntime)
	server, err := bootstrap.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A service binding must exist even when a particular request fails validation
	// before reaching it. This prevents malformed samples masking nil services.
	for name, ready := range map[string]bool{
		"catalog": cfg.Catalog.Run != nil, "pricing": cfg.Pricing.Run != nil,
		"staff authentication": cfg.Authenticator != nil, "cart": cfg.Cart != nil,
		"checkout": cfg.Checkout != nil && cfg.Checkout.Service != nil && cfg.Checkout.Resolver != nil,
		"media":    cfg.Media.Run != nil && cfg.Media.Root != "",
		"orders":   cfg.Orders.Read != nil, "fulfillment": cfg.Fulfillment.Run != nil,
		"returns": cfg.Returns.Run != nil, "CMS": cfg.CMS.Writer != nil && cfg.CMS.Idempotency.Store != nil,
		"quotes":        cfg.Quotes != nil && cfg.QuoteConversion != nil,
		"notifications": cfg.Notifications != nil, "subject": cfg.Subject != nil,
		"privacy": cfg.Privacy.Service != nil && cfg.Privacy.Enqueue != nil && cfg.Privacy.Responses != nil,
		"CSV":     cfg.CatalogCSV.Run != nil && cfg.CatalogCSV.Service != nil && cfg.CatalogCSV.Responses != nil,
	} {
		if !ready {
			t.Errorf("WIRING_GAP: %s production binding missing", name)
		}
	}

	previewToken, bearer := "", ""
	request := func(method, path, body, csrf string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, origin+path, strings.NewReader(body)).WithContext(f.ctx)
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Idempotency-Key", uuid.NewString())
		r.Header.Set("If-Match", `"1"`)
		if method == "POST" && strings.HasSuffix(path, "/quotes") {
			r.Header.Set("If-Match", "*")
		}
		if strings.Contains(path, "/catalog/csv/") {
			r.Header.Set("Content-Type", "application/octet-stream")
		}
		r.Header.Set("X-Fake-Signature", "wiring-envelope-shape-probe")
		if strings.HasSuffix(path, "/preview") && method == "GET" {
			r.Header.Set("Authorization", "Bearer "+previewToken)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
			cookies = nil
		}
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, r)
		return w
	}
	staffCookie := &http.Cookie{Name: "__Host-osc_admin_session", Value: staffToken}
	csrf := func(path string, cookie *http.Cookie) string {
		w := request("GET", path, "", "", cookie)
		var view struct {
			Token string `json:"csrf_token"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.Token == "" {
			t.Fatalf("credential setup: %s status=%d", path, w.Code)
		}
		return view.Token
	}
	staffCSRF := csrf("/api/v1/auth/session", staffCookie)
	customerCSRF := csrf("/api/v1/t/"+f.tenant+"/customer/session", customerCookie)
	draftPath := "/api/v1/t/" + f.tenant + "/sites/" + f.site + "/drafts"
	created := request("POST", draftPath, `{"locale":"en","title":"Wiring","description":""}`, staffCSRF, staffCookie)
	var draft struct {
		ID string `json:"draft_id"`
	}
	if created.Code != 201 || json.Unmarshal(created.Body.Bytes(), &draft) != nil || draft.ID == "" {
		t.Fatalf("preview draft setup status=%d", created.Code)
	}
	grant := request("POST", draftPath+"/"+draft.ID+"/preview-tokens", `{"ttl_seconds":60}`, staffCSRF, staffCookie)
	var preview struct {
		Token string `json:"token"`
	}
	if grant.Code != 201 || json.Unmarshal(grant.Body.Bytes(), &preview) != nil || preview.Token == "" {
		t.Fatalf("preview grant setup status=%d", grant.Code)
	}
	previewToken = preview.Token
	previewPath := draftPath + "/" + draft.ID + "/preview"
	challenge := request("GET", "/api/v1/auth/login", "", "")
	var challengeView struct {
		Token string `json:"csrf_token"`
	}
	if json.Unmarshal(challenge.Body.Bytes(), &challengeView) != nil || challengeView.Token == "" {
		t.Fatal("login challenge missing")
	}

	// Exact, reasoned deployment exceptions. A changed response or removed op
	// invalidates the entry; none of the discovered wiring gaps belongs here.
	allow := map[string]struct {
		status       int
		code, reason string
	}{
		"receiveStripePaymentEvent": {503, "payments_unavailable", "Stripe is mutually exclusive with this isolated Fake provider; requires sandbox deployment binding"},
		"refundReturn":              {503, "returns_unavailable", "Trusted capture/account readers and refund coordinator are out-of-band bindings; ReturnsCoordinator alone enables no HTTP refund"},
	}
	seen := map[string]bool{}
	// Embedded generator metadata normalizes IDs; the source YAML is canonical.
	doc, err := openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	specs := server.Handler.(interface{ RouteSpecs() []httpapi.RouteSpec }).RouteSpecs()
	registered := map[string]httpapi.RouteSpec{}
	for _, s := range specs {
		registered[s.OperationID] = s
	}
	type operation struct{ id, method, path, access string }
	var ops []operation
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.Extensions["x-opensite-status"] == "implemented" {
				ops = append(ops, operation{op.OperationID, method, path, fmt.Sprint(op.Extensions["x-opensite-access"])})
			}
		}
	}
	// Issue a real narrowed key through production HTTP; test both alternatives
	// on every AdminOrAPIKey operation, including issuer-to-Quotes projection.
	scopes := map[string]bool{}
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			if op.Extensions["x-opensite-status"] == "implemented" && op.Extensions["x-opensite-access"] == "AdminOrAPIKey" {
				if scope, ok := op.Extensions["x-required-scope"].(string); ok {
					scopes[scope] = true
				}
			}
		}
	}
	var keyScopes []string
	for scope := range scopes {
		keyScopes = append(keyScopes, scope)
	}
	sort.Strings(keyScopes)
	keyBody, err := json.Marshal(map[string]any{"scopes": keyScopes})
	if err != nil {
		t.Fatal(err)
	}
	keyResponse := request("POST", "/api/v1/t/"+f.tenant+"/api-keys", string(keyBody), staffCSRF, staffCookie)
	var key struct {
		Value string `json:"key"`
	}
	if keyResponse.Code != 201 || json.Unmarshal(keyResponse.Body.Bytes(), &key) != nil || key.Value == "" {
		t.Fatalf("API key setup status=%d", keyResponse.Code)
	}
	for _, cookie := range []*http.Cookie{nil, customerCookie} {
		var cookies []*http.Cookie
		if cookie != nil {
			cookies = append(cookies, cookie)
		}
		if w := request("GET", "/api/v1/t/"+f.tenant+"/quotes/"+f.quote.ID, "", "", cookies...); w.Code != 401 {
			t.Errorf("staff quote negative admission status=%d", w.Code)
		}
	}
	quoteBody := fmt.Sprintf(`{"site_id":%q,"inquiry_id":%q,"company_id":%q,"currency":"USD","expires_at":"2030-01-01T00:00:00Z","lines":[{"product_id":%q,"variant_id":%q,"description":"wiring","quantity":1,"unit_price":100,"tax_category":"standard"}],"destination":{"country":"US","subdivision":"CA"},"shipping_profile_id":%q,"weight_g":1}`, f.site, f.inquiry, f.company, f.product, f.sku, f.profile)
	// Revoke membership after route authentication but before the transaction.
	// The admitted Quotes actor must not bypass the existing staff write fence.
	// C02c sanitizes a false transaction-authority result as 401, not scope 403.
	quotePath := "/api/v1/t/" + f.tenant + "/quotes"
	changeRole := func(role string) {
		f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE app.tenant_memberships SET role=$3 WHERE tenant_id=$1 AND user_id=$2`, f.tenant, f.staff, role)
			return err
		})
	}
	revoked := false
	bodyReader := &c02cHTTPBody{reader: strings.NewReader(quoteBody), before: func() { changeRole("viewer"); revoked = true }}
	r := httptest.NewRequest("POST", origin+quotePath, bodyReader).WithContext(f.ctx)
	r.AddCookie(staffCookie)
	for name, value := range map[string]string{"Content-Type": "application/json", "Origin": origin, "X-CSRF-Token": staffCSRF, "If-Match": "*", "Idempotency-Key": uuid.NewString()} {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, r)
	changeRole("admin")
	if !revoked || w.Code != http.StatusUnauthorized {
		t.Errorf("Quotes transaction authority fence: revoked=%v status=%d", revoked, w.Code)
	}
	// Logout witnesses run last so one operation cannot invalidate another's credentials.
	sort.Slice(ops, func(i, j int) bool {
		last := func(id string) string {
			if id == "deleteCustomerSession" || id == "logoutCurrentStaffSession" {
				return "zz" + id
			}
			return id
		}
		return last(ops[i].id) < last(ops[j].id)
	})
	wildcard := regexp.MustCompile(`\{[^}]+\}`)
	for _, op := range ops {
		t.Run(op.id, func(t *testing.T) {
			if s, ok := registered[op.id]; !ok || s.Pattern != op.path || s.Method != op.method || string(s.Access) != op.access {
				t.Errorf("WIRING_GAP: absent/drifted production route %s", op.id)
				return
			}
			path := wildcard.ReplaceAllStringFunc(op.path, func(key string) string {
				switch key {
				case "{tenant_id}":
					return f.tenant
				case "{site_id}":
					return f.site
				case "{quote_id}":
					return f.quote.ID
				case "{customer_id}":
					return f.buyer
				case "{locale}":
					return "en"
				case "{min_quantity}":
					return "1"
				}
				return "11111111-1111-4111-8111-111111111111"
			})
			body, token, cookies := "", staffCSRF, []*http.Cookie{staffCookie}
			if op.method == "POST" || op.method == "PUT" || op.method == "PATCH" {
				body = "{}"
			}
			switch op.access {
			case "Public", "Storefront", "BuyerCompany", "CustomerSession":
				token, cookies = customerCSRF, []*http.Cookie{customerCookie}
			case "AdminSession", "AdminOrAPIKey", "AuthenticatedSelf":
			default:
				t.Fatalf("unreviewed access kind %s", op.access)
			}
			if op.id == "getAdminLoginChallenge" || op.id == "createStaffLoginSession" {
				token, cookies = challengeView.Token, challenge.Result().Cookies()
			}
			if op.id == "listNotificationFailures" || op.id == "redriveNotification" {
				body = ""
			}
			if op.id == "getSitePreview" {
				path = previewPath
			}
			if op.id == "createQuote" {
				body = quoteBody
			}
			if op.id == "preflightCatalogCSV" || op.id == "importCatalogCSV" {
				body = "product_code,sku,locale,title,description,lifecycle_status\nwiring,WIRING,en,Wiring,,draft\n"
			}
			if op.id == "submitInquiry" {
				body = `{"buyer_email":"wiring@example.test","message":"wiring"}`
			}
			credentials := []string{""}
			if op.access == "AdminOrAPIKey" {
				credentials = append(credentials, key.Value)
			}
			for _, credential := range credentials {
				bearer = credential
				w := request(op.method, path, body, token, cookies...)
				var problem struct{ Code, Detail string }
				_ = json.Unmarshal(w.Body.Bytes(), &problem)
				if exception, ok := allow[op.id]; ok {
					seen[op.id] = true
					if w.Code != exception.status || problem.Code != exception.code || exception.reason == "" {
						t.Errorf("stale wiring exception: status=%d code=%s", w.Code, problem.Code)
					}
					continue
				}
				if w.Code == 401 || w.Code == 403 || w.Code >= 500 || strings.Contains(problem.Detail, "Route not found") {
					t.Errorf("WIRING_GAP: %s %s status=%d code=%s detail=%s", op.access, op.id, w.Code, problem.Code, problem.Detail)
				}
			}
			bearer = ""
		})
	}
	for id := range allow {
		if !seen[id] {
			t.Errorf("stale wiring exception: operation %s absent", id)
		}
	}
	// Quotes are a runtime capability, not a side effect of enabling Fake payments.
	t.Setenv("OPENSITE_FAKE_PSP", "0")
	t.Setenv("OPENSITE_STOREFRONT_KEY", "")
	plain, closePlain, err := bootstrap.FromEnvironment(f.ctx, bootstrap.Config{Logger: cfg.Logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closePlain)
	if plain.Quotes == nil {
		t.Error("WIRING_GAP: Quotes absent without Fake checkout")
	}
}
