// Responsibility: prove CMS HTTP contracts are implemented and fail closed.
// Dependencies: generated OAS, router and publish facade; caller: Go gate.
// Invariant: signed previews and private CMS routes are explicit actual contracts.
package publish

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/internal/service"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
	"testing"
)

func TestCMSHTTPImplementedContract(t *testing.T) {
	doc, err := generated.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{"createSiteDraft": true, "getSiteDraft": true, "listDraftPages": true, "getDraftPage": true, "createDraftPage": true, "updateDraftPage": true, "deleteDraftPage": true, "updateDraftNavigation": true, "applyPageOperations": true, "validateSiteDraft": true, "publishSiteDraft": true, "createSitePreviewToken": true, "getSitePreview": true, "getSiteReleaseHistory": true, "restoreSiteRelease": true}
	for _, p := range doc.Paths.Map() {
		for _, o := range p.Operations() {
			id := o.OperationID
			if len(id) > 0 {
				id = strings.ToLower(id[:1]) + id[1:]
			}
			if required[id] {
				if o.Extensions["x-opensite-status"] != "implemented" {
					t.Errorf("%s is not implemented", id)
				}
				delete(required, id)
			}
		}
	}
	for id := range required {
		t.Errorf("missing implemented CMS contract %s", id)
	}
}

const cmsTenant = "11111111-1111-4111-8111-111111111111"
const cmsSite = "22222222-2222-4222-8222-222222222222"
const cmsDraft = "33333333-3333-4333-8333-333333333333"
const cmsPage = "44444444-4444-4444-8444-444444444444"
const cmsActor = "55555555-5555-4555-8555-555555555555"
const cmsBase = "/api/v1/t/" + cmsTenant + "/sites/" + cmsSite
const cmsDraftPath = cmsBase + "/drafts/" + cmsDraft
const cmsPreviewPath = "/api/v1/t/" + cmsTenant + "/sites/" + cmsSite + "/drafts/" + cmsDraft + "/preview"
const cmsPageBody = `{"page_key":"home","locale":"en","path":"/","document":{"title":"Home","description":"Intro","blocks":[{"kind":"heading","text":"Welcome"}]},"media_ids":[]}`

type cmsAuth struct {
	actor        string
	missingActor bool
	denyScope    string
}

func (a *cmsAuth) Authenticate(ctx context.Context, r *http.Request, s httpapi.RouteSpec) (context.Context, error) {
	if r.PathValue("tenant_id") != cmsTenant {
		return nil, apperrors.New(404, "not_found", "Resource not found")
	}
	if s.Scope == a.denyScope {
		return nil, apperrors.New(403, "forbidden", "Scope is required")
	}
	return ctx, nil
}
func (a *cmsAuth) ResolveActor(context.Context, *http.Request, httpapi.RouteSpec) (httpapi.Actor, error) {
	if a.missingActor {
		return httpapi.Actor{}, nil
	}
	if a.actor == "" {
		a.actor = cmsActor
	}
	return httpapi.Actor{Kind: "api_key", ID: a.actor}, nil
}

type cmsFakeWriter struct {
	calls, previewReads int
	revision            int64
	err                 error
	signer              *service.PreviewSigner
	last                contract.Command
}

func (w *cmsFakeWriter) Execute(ctx context.Context, c contract.Command) (contract.Result, error) {
	w.calls++
	w.last = c
	if w.err != nil {
		return contract.Result{}, w.err
	}
	if c.Operation == "list_releases" {
		return contract.Result{Releases: []contract.Release{}, PublicationRevision: 3}, nil
	}
	revision := w.revision
	if revision == 0 {
		revision = 2
	}
	page := contract.DraftPage{ID: cmsPage, PageKey: "home", Locale: "en", Path: "/", Document: contract.PageDocument{Title: "Home", Description: "", Blocks: []contract.Block{}}, MediaIDs: []string{}}
	draft := contract.Draft{DraftID: cmsDraft, SiteID: cmsSite, Revision: revision, PublicationRevision: 1, Status: "open", Locale: "en", Title: "Home", Navigation: []contract.Navigation{}, Pages: []contract.DraftPage{page}, Products: []contract.Product{}}
	result := contract.Result{Draft: &draft, Page: &page, Pages: []contract.DraftPage{page}, Validation: &contract.Validation{Valid: true, Revision: 2}, Release: &contract.Release{ReleaseID: cmsPage, ReleaseNo: 1, PublicationRevision: 2, ManifestHash: "hash", CreatedAt: "2026-10-13T00:00:00Z"}, Releases: []contract.Release{}}
	if c.Complete != nil {
		if err := c.Complete(ctx, nil, result); err != nil {
			return contract.Result{}, err
		}
	}
	return result, nil
}
func (w *cmsFakeWriter) IssuePreview(_ context.Context, c contract.Command, ttl time.Duration) (contract.PreviewGrant, error) {
	w.calls++
	token, expires, err := w.signer.Issue(c.TenantID, c.SiteID, c.DraftID, c.ExpectedRevision, ttl)
	return contract.PreviewGrant{Token: token, ExpiresAt: expires, Revision: c.ExpectedRevision}, err
}
func (w *cmsFakeWriter) Preview(_ context.Context, tenant, site, draft, token string) (contract.Site, error) {
	if _, err := w.signer.Verify(token, tenant, site, draft); err != nil {
		return contract.Site{}, err
	}
	w.previewReads++
	return contract.Site{SiteID: cmsSite, ReleaseID: cmsDraft, Hostname: "preview.invalid", Locale: "en", Title: "Preview", Navigation: []contract.Navigation{}, Pages: []contract.Page{}, Products: []contract.Product{}}, nil
}

type cmsClaim struct {
	fingerprint [32]byte
	response    *idempotency.Response
}
type cmsClaims struct {
	claims map[httpapi.CommandScope]*cmsClaim
	count  int
	store  idempotency.ResponseStore
}

func (c *cmsClaims) Claim(ctx context.Context, _ pgx.Tx, scope httpapi.CommandScope, hash [32]byte, _ time.Duration, store idempotency.ResponseStore) (httpapi.CommandCompletion, *idempotency.Response, error) {
	if old := c.claims[scope]; old != nil {
		if old.fingerprint != hash {
			return nil, nil, apperrors.New(422, "idempotency_key_reused", "Key differs")
		}
		if old.response != nil {
			return nil, old.response, nil
		}
		return nil, nil, apperrors.New(409, "idempotency_in_progress", "Command in progress")
	}
	c.count++
	entry := &cmsClaim{fingerprint: hash}
	c.claims[scope] = entry
	return func(ctx context.Context, _ pgx.Tx, response idempotency.Response) error {
		ref, err := store.Put(ctx, response)
		if err != nil {
			return err
		}
		saved, err := store.Get(ctx, ref)
		if err != nil {
			return err
		}
		entry.response = &saved
		return nil
	}, nil, nil
}
func cmsRouter(t *testing.T, auth httpapi.Authenticator, writer *cmsFakeWriter) (*httpapi.Router, *cmsClaims) {
	t.Helper()
	store, err := idempotency.NewLocalResponseStore(t.TempDir(), contract.MaxWriteBytes)
	if err != nil {
		t.Fatal(err)
	}
	claims := &cmsClaims{claims: map[httpapi.CommandScope]*cmsClaim{}, store: store}
	config := httpapi.CommandConfig{Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error { return fn(ctx, nil) }, Store: store, Claim: claims.Claim}
	router := httpapi.New(auth, nil)
	for _, route := range NewCMSHandler(CMSHTTPConfig{Writer: writer, Idempotency: config}).Routes() {
		if err = router.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	return router, claims
}
func cmsRequest(router http.Handler, method, path, body, key, version string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if version != "" {
		req.Header.Set("If-Match", version)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}
func TestCMSHTTPAdmissionAndRedaction(t *testing.T) {
	writer := &cmsFakeWriter{}
	auth := &cmsAuth{}
	router, claims := cmsRouter(t, auth, writer)
	for _, tc := range []struct {
		name, path, body, key, version string
		status                         int
	}{
		{"version missing", cmsDraftPath + "/pages", cmsPageBody, "a", "", 428},
		{"version unquoted", cmsDraftPath + "/pages", cmsPageBody, "a", "1", 400},
		{"unknown field", cmsDraftPath + "/pages", strings.Replace(cmsPageBody, `"page_key"`, `"unknown":1,"page_key"`, 1), "a", `"1"`, 400},
		{"duplicate field", cmsDraftPath + "/pages", strings.Replace(cmsPageBody, `"locale":"en"`, `"locale":"en","locale":"zh"`, 1), "a", `"1"`, 400},
		{"unknown block", cmsDraftPath + "/pages", strings.Replace(cmsPageBody, `"heading"`, `"html"`, 1), "a", `"1"`, 400},
		{"script text", cmsDraftPath + "/pages", strings.Replace(cmsPageBody, "Welcome", "<script>alert(1)</script>", 1), "a", `"1"`, 422},
		{"reserved route", cmsDraftPath + "/pages", strings.Replace(cmsPageBody, `"path":"/"`, `"path":"/products/secret"`, 1), "a", `"1"`, 422},
		{"query", cmsDraftPath + "/pages?token=secret", cmsPageBody, "a", `"1"`, 400},
		{"key missing", cmsDraftPath + "/pages", cmsPageBody, "", `"1"`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := cmsRequest(router, "POST", tc.path, tc.body, tc.key, tc.version)
			if w.Code != tc.status || writer.calls != 0 || claims.count != 0 {
				t.Fatalf("status=%d service=%d claims=%d body=%s", w.Code, writer.calls, claims.count, w.Body)
			}
		})
	}
	wbody := cmsRequest(router, "GET", cmsDraftPath, `{"actor":"spoof"}`, "", "")
	if wbody.Code != 400 || writer.calls != 0 {
		t.Fatalf("GET body %d calls=%d", wbody.Code, writer.calls)
	}
	auth.missingActor = true
	w := cmsRequest(router, "POST", cmsDraftPath+"/pages", cmsPageBody, "a", `"1"`)
	if w.Code != 401 {
		t.Fatalf("missing actor=%d", w.Code)
	}
	auth.missingActor = false
	auth.denyScope = "cms:write"
	w = cmsRequest(router, "POST", cmsDraftPath+"/pages", cmsPageBody, "a", `"1"`)
	if w.Code != 403 {
		t.Fatalf("scope=%d", w.Code)
	}
	auth.denyScope = ""
	w = cmsRequest(router, "GET", strings.Replace(cmsDraftPath, cmsTenant, cmsActor, 1), "", "", "")
	if w.Code != 404 {
		t.Fatalf("tenant=%d", w.Code)
	}
	writer.err = errors.New("private-token-dsn-password")
	w = cmsRequest(router, "GET", cmsDraftPath, "", "", "")
	if w.Code != 503 || strings.Contains(w.Body.String(), "private-token") {
		t.Fatalf("unredacted response %d %s", w.Code, w.Body)
	}
	denied, _ := cmsRouter(t, nil, writer)
	w = cmsRequest(denied, "POST", cmsDraftPath+"/pages", cmsPageBody, "a", `"1"`)
	if w.Code != 401 {
		t.Fatalf("production default auth=%d", w.Code)
	}
}
func TestCMSHTTPExactReplayAndScope(t *testing.T) {
	writer := &cmsFakeWriter{}
	auth := &cmsAuth{}
	router, claims := cmsRouter(t, auth, writer)
	first := cmsRequest(router, "POST", cmsDraftPath+"/pages", cmsPageBody, "exact", `"1"`)
	if first.Code != 201 || first.Header().Get("Cache-Control") != "no-store" || first.Header().Get("ETag") != `"2"` {
		t.Fatalf("first %d %s", first.Code, first.Body)
	}
	writer.revision = 7
	second := cmsRequest(router, "POST", cmsDraftPath+"/pages", cmsPageBody, "exact", `"1"`)
	if second.Code != first.Code || second.Body.String() != first.Body.String() || second.Header().Get("Cache-Control") != first.Header().Get("Cache-Control") || second.Header().Get("ETag") != first.Header().Get("ETag") || writer.calls != 1 {
		t.Fatalf("replay %d calls=%d", second.Code, writer.calls)
	}
	changed := cmsRequest(router, "POST", cmsDraftPath+"/pages", strings.Replace(cmsPageBody, "Welcome", "Changed", 1), "exact", `"1"`)
	if changed.Code != 422 || writer.calls != 1 {
		t.Fatalf("changed %d calls=%d", changed.Code, writer.calls)
	}
	changed = cmsRequest(router, "POST", cmsDraftPath+"/pages", cmsPageBody, "exact", `"2"`)
	if changed.Code != 422 {
		t.Fatalf("changed IfMatch %d", changed.Code)
	}
	changed = cmsRequest(router, "POST", strings.Replace(cmsDraftPath, cmsDraft, cmsPage, 1)+"/pages", cmsPageBody, "exact", `"1"`)
	if changed.Code != 422 || writer.calls != 1 {
		t.Fatalf("path scope %d calls=%d", changed.Code, writer.calls)
	}
	key1 := writer.last.ReceiptKey
	auth.actor = cmsPage
	changed = cmsRequest(router, "POST", cmsDraftPath+"/pages", cmsPageBody, "exact", `"1"`)
	if changed.Code != 201 || writer.last.ReceiptKey == key1 || claims.count != 2 {
		t.Fatalf("actor isolation %d claims=%d", changed.Code, claims.count)
	}
	writer.err = contract.ErrConflict
	changed = cmsRequest(router, "POST", cmsDraftPath+"/pages", cmsPageBody, "inflight", `"1"`)
	if changed.Code != 412 {
		t.Fatalf("stale version %d", changed.Code)
	}
	changed = cmsRequest(router, "POST", cmsDraftPath+"/pages", cmsPageBody, "inflight", `"1"`)
	if changed.Code != 409 {
		t.Fatalf("in-progress %d", changed.Code)
	}
}
func TestCMSHTTPPreviewSignedReadOnly(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	signer, err := service.NewPreviewSigner([]byte(strings.Repeat("k", 32)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	writer := &cmsFakeWriter{signer: signer}
	router, _ := cmsRouter(t, nil, writer)
	token, _, err := signer.Issue(cmsTenant, cmsSite, cmsDraft, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	send := func(path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	for _, bad := range []string{"", "short", token[:len(token)-2] + "xx"} {
		w := send(cmsPreviewPath, bad)
		if w.Code != 403 || writer.previewReads != 0 {
			t.Fatalf("bad preview %d reads=%d", w.Code, writer.previewReads)
		}
	}
	w := send(cmsPreviewPath+"?token="+token, token)
	if w.Code != 400 || writer.previewReads != 0 {
		t.Fatalf("query credential %d reads=%d", w.Code, writer.previewReads)
	}
	w = send(cmsPreviewPath, token)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Robots-Tag") != "noindex, nofollow" || writer.previewReads != 1 || writer.calls != 0 {
		t.Fatalf("preview %d %s reads=%d", w.Code, w.Body, writer.previewReads)
	}
	// G6: the preview DTO is not the public storefront body (no tenant_id, no SKUs).
	var preview map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil || preview["tenant_id"] != nil || preview["hostname"] == nil || strings.Contains(w.Body.String(), `"skus"`) {
		t.Fatalf("preview DTO: %s %v", w.Body, err)
	}
	now = now.Add(time.Minute)
	w = send(cmsPreviewPath, token)
	if w.Code != 403 || writer.previewReads != 1 {
		t.Fatalf("expiry %d reads=%d", w.Code, writer.previewReads)
	}
}
func TestCMSHTTPTransportBounds(t *testing.T) {
	writer := &cmsFakeWriter{}
	router, claims := cmsRouter(t, &cmsAuth{}, writer)
	r := httptest.NewRequest("POST", cmsDraftPath+"/pages", strings.NewReader(cmsPageBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "a")
	r.Header.Add("Idempotency-Key", "b")
	r.Header.Set("If-Match", `"1"`)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("duplicate key %d", w.Code)
	}
	r = httptest.NewRequest("POST", cmsDraftPath+"/pages", strings.NewReader(cmsPageBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "a")
	r.Header.Add("If-Match", `"1"`)
	r.Header.Add("If-Match", `"2"`)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("duplicate version %d", w.Code)
	}
	r = httptest.NewRequest("POST", cmsDraftPath+"/pages", nil)
	r.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", contract.MaxWriteBytes+1)))
	r.ContentLength = -1
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "a")
	r.Header.Set("If-Match", `"1"`)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 413 || writer.calls != 0 || claims.count != 0 {
		t.Fatalf("chunked bound %d calls=%d claims=%d", w.Code, writer.calls, claims.count)
	}
}
func TestCMSHTTPEveryOperationReachesFacade(t *testing.T) {
	signer, _ := service.NewPreviewSigner([]byte(strings.Repeat("k", 32)), time.Now)
	writer := &cmsFakeWriter{signer: signer}
	router, _ := cmsRouter(t, &cmsAuth{}, writer)
	for i, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", cmsBase + "/drafts", `{"locale":"en","title":"Shop","description":""}`, 201},
		{"GET", cmsDraftPath, "", 200}, {"GET", cmsDraftPath + "/pages", "", 200}, {"GET", cmsDraftPath + "/pages/" + cmsPage, "", 200},
		{"POST", cmsDraftPath + "/pages", cmsPageBody, 201}, {"PUT", cmsDraftPath + "/pages/" + cmsPage, cmsPageBody, 200}, {"DELETE", cmsDraftPath + "/pages/" + cmsPage, `{"locale":"en"}`, 200},
		{"PUT", cmsDraftPath + "/navigation", `{"navigation":[]}`, 200}, {"POST", cmsDraftPath + "/operations", `{"operations":[{"operation":"save_page","payload":` + cmsPageBody + `}]}`, 200},
		{"POST", cmsDraftPath + "/validate", "", 200}, {"POST", cmsDraftPath + "/publish", `{"expected_release_id":null,"expected_publication_revision":1,"renderer_build_id":"test"}`, 201},
		{"POST", cmsDraftPath + "/preview-tokens", `{"ttl_seconds":60}`, 201}, {"GET", cmsBase + "/releases", "", 200}, {"POST", cmsBase + "/releases/" + cmsPage + "/restore", `{"expected_release_id":null}`, 201},
	} {
		before := writer.calls
		w := cmsRequest(router, tc.method, tc.path, tc.body, "op-"+string(rune('a'+i)), `"1"`)
		if w.Code != tc.status || writer.calls != before+1 {
			t.Fatalf("%s %s: %d calls=%d body=%s", tc.method, tc.path, w.Code, writer.calls, w.Body)
		}
		var raw any
		if json.Unmarshal(w.Body.Bytes(), &raw) != nil {
			t.Fatal("not JSON")
		}
	}
}

func TestCMSHTTPUpdateRequiresExistingDraftPage(t *testing.T) {
	writer := &cmsFakeWriter{}
	router, _ := cmsRouter(t, &cmsAuth{}, writer)
	w := cmsRequest(router, "PUT", cmsDraftPath+"/pages/"+cmsPage, cmsPageBody, "update-membership", `"1"`)
	if w.Code != 200 || writer.last.Operation != "update_page" || writer.last.PageID != cmsPage {
		t.Fatalf("update must retain draft membership semantics: status=%d operation=%s page=%s", w.Code, writer.last.Operation, writer.last.PageID)
	}
}

// Entity decoding is not idempotent: the HTTP edge must hand the Writer the
// original payload so the real Writer sanitizes exactly once.
func TestCMSHTTPSanitizesOnce(t *testing.T) {
	writer := &cmsFakeWriter{}
	router, _ := cmsRouter(t, &cmsAuth{}, writer)
	body := strings.Replace(cmsPageBody, `"Welcome"`, `"<p>&amp;copy; &amp;lt;b&amp;gt;</p>"`, 1)
	if w := cmsRequest(router, "POST", cmsDraftPath+"/pages", body, "once", `"1"`); w.Code != 201 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	// Second (Writer-side) pass over what the handler forwarded.
	stored, err := validateCommand(writer.last)
	if err != nil {
		t.Fatal("writer rejected forwarded payload:", err)
	}
	var page service.SavePage
	if err = json.Unmarshal(stored.Payload, &page); err != nil || page.Document.Blocks[0].Text != "&copy; &lt;b&gt;" {
		t.Fatalf("text sanitized twice: %s %v", stored.Payload, err)
	}
}

func TestCMSHTTPReleaseHistoryETag(t *testing.T) {
	router, _ := cmsRouter(t, &cmsAuth{}, &cmsFakeWriter{})
	w := cmsRequest(router, "GET", cmsBase+"/releases", "", "", "")
	if w.Code != 200 || w.Header().Get("ETag") != `"3"` {
		t.Fatalf("status=%d etag=%q", w.Code, w.Header().Get("ETag"))
	}
}
