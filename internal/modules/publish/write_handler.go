// Responsibilities: expose strict CMS drafts, publication and signed previews.
// Dependencies: publish contract/validation and central authenticated C-04 transport.
// Callers: API bootstrap. Invariants: actor/tenant are stamped, completion shares
// the mutation commit, bearer previews are read-only and never cached/indexed.
package publish

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type CMSHTTPConfig struct {
	Writer      contract.Writer
	Idempotency httpapi.CommandConfig
}
type CMSHandler struct {
	httpapi.StrictServerInterface
	config CMSHTTPConfig
}

func NewCMSHandler(c CMSHTTPConfig) *CMSHandler { return &CMSHandler{config: c} }
func (s *CMSHandler) Routes() []httpapi.Route {
	const base = "/api/v1/t/{tenant_id}/sites/{site_id}"
	specs := []httpapi.RouteSpec{
		{OperationID: "createSiteDraft", Method: "POST", Pattern: base + "/drafts"},
		{OperationID: "getSiteDraft", Method: "GET", Pattern: base + "/drafts/{draft_id}"},
		{OperationID: "listDraftPages", Method: "GET", Pattern: base + "/drafts/{draft_id}/pages"},
		{OperationID: "createDraftPage", Method: "POST", Pattern: base + "/drafts/{draft_id}/pages"},
		{OperationID: "getDraftPage", Method: "GET", Pattern: base + "/drafts/{draft_id}/pages/{page_id}"},
		{OperationID: "updateDraftPage", Method: "PUT", Pattern: base + "/drafts/{draft_id}/pages/{page_id}"},
		{OperationID: "deleteDraftPage", Method: "DELETE", Pattern: base + "/drafts/{draft_id}/pages/{page_id}"},
		{OperationID: "updateDraftNavigation", Method: "PUT", Pattern: base + "/drafts/{draft_id}/navigation"},
		{OperationID: "applyPageOperations", Method: "POST", Pattern: base + "/drafts/{draft_id}/operations"},
		{OperationID: "validateSiteDraft", Method: "POST", Pattern: base + "/drafts/{draft_id}/validate"},
		{OperationID: "publishSiteDraft", Method: "POST", Pattern: base + "/drafts/{draft_id}/publish"},
		{OperationID: "createSitePreviewToken", Method: "POST", Pattern: base + "/drafts/{draft_id}/preview-tokens"},
		{OperationID: "getSiteReleaseHistory", Method: "GET", Pattern: base + "/releases"},
		{OperationID: "restoreSiteRelease", Method: "POST", Pattern: base + "/releases/{release_id}/restore"},
		{OperationID: "getSitePreview", Method: "GET", Pattern: "/api/v1/t/{tenant_id}/sites/{site_id}/drafts/{draft_id}/preview", Access: httpapi.Public},
	}
	for i := range specs {
		if specs[i].Access != httpapi.Public {
			specs[i].Access = httpapi.AdminOrAPIKey
			specs[i].Scope = "cms:write"
			if specs[i].OperationID == "publishSiteDraft" || specs[i].OperationID == "restoreSiteRelease" {
				specs[i].Scope = "cms:publish"
			}
		}
		specs[i].MaxBodySize = contract.MaxWriteBytes
	}
	routes := httpapi.StrictRoutes(s, specs)
	for i := range routes {
		next := routes[i].Handler
		if routes[i].Spec.Method != "GET" {
			next = httpapi.CaptureCommand(routes[i].Spec, next)
		}
		preview := routes[i].Spec.OperationID == "getSitePreview"
		noBody := routes[i].Spec.Method == "GET" || routes[i].Spec.OperationID == "validateSiteDraft"
		routes[i].Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if preview {
				w.Header().Set("X-Robots-Tag", "noindex, nofollow")
				values := r.Header.Values("Authorization")
				if len(values) != 1 || (!strings.HasPrefix(values[0], "Bearer ") || len(values[0]) < 20 || len(values[0]) > 4096) {
					httpapi.WriteProblem(w, r, cmsError(contract.ErrPreviewDenied))
					return
				}
			}
			if r.URL.RawQuery != "" {
				httpapi.WriteProblem(w, r, apperrors.New(400, "invalid_request", "Query parameters are not supported"))
				return
			}
			if noBody && r.Body != nil && r.Body != http.NoBody {
				raw, err := io.ReadAll(io.LimitReader(r.Body, 1))
				if err != nil || len(raw) > 0 {
					httpapi.WriteProblem(w, r, apperrors.New(400, "invalid_request", "This operation does not accept a request body"))
					return
				}
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	return routes
}
func cmsError(err error) error {
	if err == nil {
		return nil
	}
	var e *apperrors.Error
	if errors.As(err, &e) {
		return e
	}
	switch {
	case errors.Is(err, contract.ErrNotFound):
		return apperrors.New(404, "not_found", "Resource not found")
	case errors.Is(err, contract.ErrConflict):
		return apperrors.New(412, "precondition_failed", "CMS revision changed")
	case errors.Is(err, contract.ErrInvalid):
		return apperrors.New(422, "invalid_cms", "Invalid CMS input")
	case errors.Is(err, contract.ErrPreviewDenied):
		return apperrors.New(403, "preview_denied", "Preview grant is invalid or expired")
	default:
		return apperrors.New(503, "cms_unavailable", "CMS service is unavailable")
	}
}
func cmsVersion(v string) int64 {
	if len(v) < 3 {
		return 0
	}
	n, _ := strconv.ParseInt(v[1:len(v)-1], 10, 64)
	return n
}
func cmsPayload(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func cmsDTO[T any](v any) (T, error) {
	var dst T
	b, err := json.Marshal(v)
	if err == nil {
		err = json.Unmarshal(b, &dst)
	}
	return dst, err
}
func cmsResultValue(op string, r contract.Result) (any, error) {
	switch op {
	case "get_draft", "create_draft", "set_navigation", "apply_operations", "delete_page":
		if r.Draft != nil {
			return cmsDTO[httpapi.CMSDraft](r.Draft)
		}
	case "get_page", "save_page", "update_page":
		if r.Page != nil {
			return cmsDTO[httpapi.CMSDraftPage](r.Page)
		}
	case "list_pages":
		if r.Pages == nil {
			r.Pages = []contract.DraftPage{}
		}
		return cmsDTO[httpapi.CMSDraftPageList](r.Pages)
	case "list_releases":
		if r.Releases == nil {
			r.Releases = []contract.Release{}
		}
		return cmsDTO[httpapi.CMSReleaseList](r.Releases)
	case "publish_draft", "restore_release":
		if r.Release != nil {
			return cmsDTO[httpapi.CMSRelease](r.Release)
		}
	case "validate_draft":
		if r.Validation != nil {
			return cmsDTO[httpapi.CMSValidation](r.Validation)
		}
	}
	return nil, contract.ErrUnavailable
}
func cmsEncode(status int, v any) (idempotency.Response, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return idempotency.Response{}, err
	}
	raw = append(raw, '\n')
	if len(raw) > contract.MaxWriteBytes {
		return idempotency.Response{}, contract.ErrUnavailable
	}
	response := idempotency.Response{Status: status, Header: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: raw}
	var version struct {
		Revision            int64 `json:"revision"`
		PublicationRevision int64 `json:"publication_revision"`
	}
	if json.Unmarshal(raw, &version) == nil {
		n := version.Revision
		if n == 0 {
			n = version.PublicationRevision
		}
		if n > 0 {
			response.Header.Set("ETag", strconv.Quote(strconv.FormatInt(n, 10)))
		}
	}
	return response, nil
}

// Page representations use the containing draft revision, not a page-local version.
func cmsResultETag(response *idempotency.Response, result contract.Result) {
	if result.Draft != nil {
		response.Header.Set("ETag", strconv.Quote(strconv.FormatInt(result.Draft.Revision, 10)))
	}
}

// Saved headers and body are identical on first response and C-04 replay.
func cmsWrite(w http.ResponseWriter, r idempotency.Response) error {
	for name := range r.Header {
		w.Header().Del(name)
	}
	return httpapi.WriteCommandResponse(w, r)
}

type cmsResponse struct{ response idempotency.Response }

func (s *CMSHandler) execute(ctx context.Context, operation string, c contract.Command, status int, mutation bool) (cmsResponse, error) {
	if _, err := authorizeCMS(ctx, c); err != nil {
		return cmsResponse{}, err
	}
	if s.config.Writer == nil {
		return cmsResponse{}, cmsError(contract.ErrUnavailable)
	}
	var validationError error
	c, validationError = validateCommand(c)
	if validationError != nil {
		return cmsResponse{}, cmsError(validationError)
	}
	if !mutation {
		result, err := s.config.Writer.Execute(ctx, c)
		if err != nil {
			return cmsResponse{}, cmsError(err)
		}
		v, err := cmsResultValue(c.Operation, result)
		if err != nil {
			return cmsResponse{}, cmsError(err)
		}
		r, err := cmsEncode(status, v)
		cmsResultETag(&r, result)
		return cmsResponse{r}, cmsError(err)
	}
	session, replay, err := httpapi.BeginCommand(ctx, s.config.Idempotency, operation)
	if err != nil {
		return cmsResponse{}, err
	}
	if replay != nil {
		return cmsResponse{*replay}, nil
	}
	c.ReceiptKey = session.Key()
	c.Complete = func(txctx context.Context, tx pgx.Tx, result contract.Result) error {
		v, err := cmsResultValue(c.Operation, result)
		if err != nil {
			return err
		}
		response, err := cmsEncode(status, v)
		if err != nil {
			return err
		}
		cmsResultETag(&response, result)
		return session.CompleteResponse(txctx, tx, response)
	}
	_, err = s.config.Writer.Execute(ctx, c)
	if err != nil {
		return cmsResponse{}, cmsError(err)
	}
	response := session.Response()
	if response.Status == 0 {
		return cmsResponse{}, cmsError(contract.ErrUnavailable)
	}
	return cmsResponse{response}, nil
}

func (s *CMSHandler) CreateSiteDraft(ctx context.Context, r httpapi.CreateSiteDraftRequestObject) (httpapi.CreateSiteDraftResponseObject, error) {
	c := contract.Command{Operation: "create_draft", TenantID: r.TenantId.String(), SiteID: r.SiteId.String()}
	if r.Body == nil {
		return nil, cmsError(contract.ErrInvalid)
	}
	c.Payload = cmsPayload(r.Body)
	return s.execute(ctx, "createSiteDraft", c, 201, true)
}

func (s *CMSHandler) GetSiteDraft(ctx context.Context, r httpapi.GetSiteDraftRequestObject) (httpapi.GetSiteDraftResponseObject, error) {
	c := contract.Command{Operation: "get_draft", TenantID: r.TenantId.String(), SiteID: r.SiteId.String()}
	c.DraftID = r.DraftId.String()
	return s.execute(ctx, "getSiteDraft", c, 200, false)
}

func (s *CMSHandler) ListDraftPages(ctx context.Context, r httpapi.ListDraftPagesRequestObject) (httpapi.ListDraftPagesResponseObject, error) {
	c := contract.Command{Operation: "list_pages", TenantID: r.TenantId.String(), SiteID: r.SiteId.String()}
	c.DraftID = r.DraftId.String()
	return s.execute(ctx, "listDraftPages", c, 200, false)
}

func (s *CMSHandler) GetDraftPage(ctx context.Context, r httpapi.GetDraftPageRequestObject) (httpapi.GetDraftPageResponseObject, error) {
	c := contract.Command{Operation: "get_page", TenantID: r.TenantId.String(), SiteID: r.SiteId.String()}
	c.DraftID = r.DraftId.String()
	c.PageID = r.PageId.String()
	return s.execute(ctx, "getDraftPage", c, 200, false)
}

func (s *CMSHandler) CreateDraftPage(ctx context.Context, r httpapi.CreateDraftPageRequestObject) (httpapi.CreateDraftPageResponseObject, error) {
	c := contract.Command{Operation: "save_page", TenantID: r.TenantId.String(), SiteID: r.SiteId.String()}
	c.DraftID = r.DraftId.String()
	c.ExpectedRevision = cmsVersion(r.Params.IfMatch)
	if r.Body == nil {
		return nil, cmsError(contract.ErrInvalid)
	}
	c.Payload = cmsPayload(r.Body)
	return s.execute(ctx, "createDraftPage", c, 201, true)
}

func (s *CMSHandler) UpdateDraftPage(ctx context.Context, r httpapi.UpdateDraftPageRequestObject) (httpapi.UpdateDraftPageResponseObject, error) {
	c := contract.Command{Operation: "update_page", TenantID: r.TenantId.String(), SiteID: r.SiteId.String()}
	c.DraftID = r.DraftId.String()
	c.ExpectedRevision = cmsVersion(r.Params.IfMatch)
	c.PageID = r.PageId.String()
	if r.Body == nil {
		return nil, cmsError(contract.ErrInvalid)
	}
	var body map[string]any
	_ = json.Unmarshal(cmsPayload(r.Body), &body)
	body["id"] = c.PageID
	c.Payload = cmsPayload(body)
	return s.execute(ctx, "updateDraftPage", c, 200, true)
}

func (s *CMSHandler) DeleteDraftPage(ctx context.Context, r httpapi.DeleteDraftPageRequestObject) (httpapi.DeleteDraftPageResponseObject, error) {
	c := contract.Command{Operation: "delete_page", TenantID: r.TenantId.String(), SiteID: r.SiteId.String()}
	c.DraftID = r.DraftId.String()
	c.ExpectedRevision = cmsVersion(r.Params.IfMatch)
	c.PageID = r.PageId.String()
	if r.Body == nil {
		return nil, cmsError(contract.ErrInvalid)
	}
	c.Payload = cmsPayload(map[string]string{"id": c.PageID, "locale": r.Body.Locale})
	return s.execute(ctx, "deleteDraftPage", c, 200, true)
}

func (s *CMSHandler) UpdateDraftNavigation(ctx context.Context, r httpapi.UpdateDraftNavigationRequestObject) (httpapi.UpdateDraftNavigationResponseObject, error) {
	c := contract.Command{Operation: "set_navigation", TenantID: r.TenantId.String(), SiteID: r.SiteId.String()}
	c.DraftID = r.DraftId.String()
	c.ExpectedRevision = cmsVersion(r.Params.IfMatch)
	if r.Body == nil {
		return nil, cmsError(contract.ErrInvalid)
	}
	c.Payload = cmsPayload(r.Body)
	return s.execute(ctx, "updateDraftNavigation", c, 200, true)
}

func (s *CMSHandler) ApplyPageOperations(ctx context.Context, r httpapi.ApplyPageOperationsRequestObject) (httpapi.ApplyPageOperationsResponseObject, error) {
	c := contract.Command{Operation: "apply_operations", TenantID: r.TenantId.String(), SiteID: r.SiteId.String()}
	c.DraftID = r.DraftId.String()
	c.ExpectedRevision = cmsVersion(r.Params.IfMatch)
	if r.Body == nil {
		return nil, cmsError(contract.ErrInvalid)
	}
	c.Payload = cmsPayload(r.Body)
	return s.execute(ctx, "applyPageOperations", c, 200, true)
}

func (s *CMSHandler) ValidateSiteDraft(ctx context.Context, r httpapi.ValidateSiteDraftRequestObject) (httpapi.ValidateSiteDraftResponseObject, error) {
	c := contract.Command{Operation: "validate_draft", TenantID: r.TenantId.String(), SiteID: r.SiteId.String()}
	c.DraftID = r.DraftId.String()
	c.ExpectedRevision = cmsVersion(r.Params.IfMatch)
	return s.execute(ctx, "validateSiteDraft", c, 200, true)
}

func (s *CMSHandler) PublishSiteDraft(ctx context.Context, r httpapi.PublishSiteDraftRequestObject) (httpapi.PublishSiteDraftResponseObject, error) {
	c := contract.Command{Operation: "publish_draft", TenantID: r.TenantId.String(), SiteID: r.SiteId.String()}
	c.DraftID = r.DraftId.String()
	c.ExpectedRevision = cmsVersion(r.Params.IfMatch)
	if r.Body == nil {
		return nil, cmsError(contract.ErrInvalid)
	}
	var body struct {
		ExpectedReleaseID *string `json:"expected_release_id"`
	}
	_ = json.Unmarshal(cmsPayload(r.Body), &body)
	if body.ExpectedReleaseID != nil {
		c.ExpectedReleaseID = *body.ExpectedReleaseID
	}
	c.ExpectedPublicationRevision = r.Body.ExpectedPublicationRevision
	c.Payload = cmsPayload(map[string]string{"renderer_build_id": r.Body.RendererBuildId})
	return s.execute(ctx, "publishSiteDraft", c, 201, true)
}

func (s *CMSHandler) GetSiteReleaseHistory(ctx context.Context, r httpapi.GetSiteReleaseHistoryRequestObject) (httpapi.GetSiteReleaseHistoryResponseObject, error) {
	c := contract.Command{Operation: "list_releases", TenantID: r.TenantId.String(), SiteID: r.SiteId.String()}
	return s.execute(ctx, "getSiteReleaseHistory", c, 200, false)
}

func (s *CMSHandler) RestoreSiteRelease(ctx context.Context, r httpapi.RestoreSiteReleaseRequestObject) (httpapi.RestoreSiteReleaseResponseObject, error) {
	c := contract.Command{Operation: "restore_release", TenantID: r.TenantId.String(), SiteID: r.SiteId.String()}
	c.ExpectedRevision = cmsVersion(r.Params.IfMatch)
	if r.Body == nil {
		return nil, cmsError(contract.ErrInvalid)
	}
	var body struct {
		ExpectedReleaseID *string `json:"expected_release_id"`
	}
	_ = json.Unmarshal(cmsPayload(r.Body), &body)
	if body.ExpectedReleaseID != nil {
		c.ExpectedReleaseID = *body.ExpectedReleaseID
	}
	c.ReleaseID = r.ReleaseId.String()
	c.ExpectedPublicationRevision = c.ExpectedRevision
	return s.execute(ctx, "restoreSiteRelease", c, 201, true)
}
func (r cmsResponse) VisitCreateSiteDraftResponse(w http.ResponseWriter) error {
	return cmsWrite(w, r.response)
}
func (r cmsResponse) VisitGetSiteDraftResponse(w http.ResponseWriter) error {
	return cmsWrite(w, r.response)
}
func (r cmsResponse) VisitListDraftPagesResponse(w http.ResponseWriter) error {
	return cmsWrite(w, r.response)
}
func (r cmsResponse) VisitGetDraftPageResponse(w http.ResponseWriter) error {
	return cmsWrite(w, r.response)
}
func (r cmsResponse) VisitCreateDraftPageResponse(w http.ResponseWriter) error {
	return cmsWrite(w, r.response)
}
func (r cmsResponse) VisitUpdateDraftPageResponse(w http.ResponseWriter) error {
	return cmsWrite(w, r.response)
}
func (r cmsResponse) VisitDeleteDraftPageResponse(w http.ResponseWriter) error {
	return cmsWrite(w, r.response)
}
func (r cmsResponse) VisitUpdateDraftNavigationResponse(w http.ResponseWriter) error {
	return cmsWrite(w, r.response)
}
func (r cmsResponse) VisitApplyPageOperationsResponse(w http.ResponseWriter) error {
	return cmsWrite(w, r.response)
}
func (r cmsResponse) VisitValidateSiteDraftResponse(w http.ResponseWriter) error {
	return cmsWrite(w, r.response)
}
func (r cmsResponse) VisitPublishSiteDraftResponse(w http.ResponseWriter) error {
	return cmsWrite(w, r.response)
}
func (r cmsResponse) VisitGetSiteReleaseHistoryResponse(w http.ResponseWriter) error {
	return cmsWrite(w, r.response)
}
func (r cmsResponse) VisitRestoreSiteReleaseResponse(w http.ResponseWriter) error {
	return cmsWrite(w, r.response)
}
func (r cmsResponse) VisitCreateSitePreviewTokenResponse(w http.ResponseWriter) error {
	return cmsWrite(w, r.response)
}
func (r cmsResponse) VisitGetSitePreviewResponse(w http.ResponseWriter) error {
	return cmsWrite(w, r.response)
}

func (s *CMSHandler) CreateSitePreviewToken(ctx context.Context, r httpapi.CreateSitePreviewTokenRequestObject) (httpapi.CreateSitePreviewTokenResponseObject, error) {
	tenant := r.TenantId.String()
	c := contract.Command{TenantID: tenant, SiteID: r.SiteId.String(), DraftID: r.DraftId.String(), ExpectedRevision: cmsVersion(r.Params.IfMatch)}
	if _, err := authorizeCMS(ctx, c); err != nil {
		return nil, err
	}
	if r.Body == nil || r.Body.TtlSeconds < 1 || time.Duration(r.Body.TtlSeconds)*time.Second > contract.MaxPreviewTTL {
		return nil, cmsError(contract.ErrInvalid)
	}
	if s.config.Writer == nil {
		return nil, cmsError(contract.ErrUnavailable)
	}
	session, replay, err := httpapi.BeginCommand(ctx, s.config.Idempotency, "createSitePreviewToken")
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return cmsResponse{*replay}, nil
	}
	grant, err := s.config.Writer.IssuePreview(ctx, c, time.Duration(r.Body.TtlSeconds)*time.Second)
	if err != nil {
		return nil, cmsError(err)
	}
	dto, err := cmsDTO[httpapi.CMSPreviewGrant](grant)
	if err != nil {
		return nil, cmsError(err)
	}
	response, err := cmsEncode(201, dto)
	if err != nil {
		return nil, cmsError(err)
	}
	err = s.config.Idempotency.Run(ctx, tenant, func(txctx context.Context, tx pgx.Tx) error { return session.CompleteResponse(txctx, tx, response) })
	if err != nil {
		return nil, cmsError(err)
	}
	return cmsResponse{session.Response()}, nil
}
func (s *CMSHandler) GetSitePreview(ctx context.Context, r httpapi.GetSitePreviewRequestObject) (httpapi.GetSitePreviewResponseObject, error) {
	token, ok := strings.CutPrefix(r.Params.Authorization, "Bearer ")
	if !ok || token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, " \t\r\n") {
		return nil, cmsError(contract.ErrPreviewDenied)
	}
	if s.config.Writer == nil {
		return nil, cmsError(contract.ErrUnavailable)
	}
	site, err := s.config.Writer.Preview(ctx, r.TenantId.String(), r.SiteId.String(), r.DraftId.String(), token)
	if err != nil {
		return nil, cmsError(err)
	}
	dto, err := cmsDTO[httpapi.PublishedSite](site)
	if err != nil {
		return nil, cmsError(err)
	}
	response, err := cmsEncode(200, dto)
	if err != nil {
		return nil, cmsError(err)
	}
	response.Header.Set("X-Robots-Tag", "noindex, nofollow")
	return cmsResponse{response}, nil
}
