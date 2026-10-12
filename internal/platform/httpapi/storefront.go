// Responsibility: anonymous strict generated Read Context HTTP transport.
// Dependencies: publish public contract and generated OAS; caller: cmd/api bootstrap.
// Invariants: exactly one validated host, bounded read deadline, no forwarded-host trust.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	gen "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
	"net/http"
	"net/url"
	"time"
)

type storefrontHandler struct {
	gen.StrictServerInterface
	reader contract.Reader
}

func StorefrontRoute(reader contract.Reader) Route {
	s := &storefrontHandler{reader: reader}
	strict := gen.Handler(gen.NewStrictHandlerWithOptions(s, nil, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			WriteProblem(w, r, apperrors.New(400, "invalid_request", "Invalid public query"))
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) { WriteProblem(w, r, err) },
	}))
	return Route{Spec: RouteSpec{OperationID: "getPublishedSite", Method: "GET", Pattern: "/api/v1/public/sites/current", Access: Public}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(query) != 1 || len(query["host"]) != 1 {
			WriteProblem(w, r, apperrors.New(400, "invalid_request", "Exactly one host is required"))
			return
		}
		if _, err := contract.NormalizeHost(query.Get("host")); err != nil {
			WriteProblem(w, r, apperrors.New(400, "invalid_request", "Invalid DNS hostname"))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		strict.ServeHTTP(w, r.WithContext(ctx))
	})}
}
func (s *storefrontHandler) GetPublishedSite(ctx context.Context, req gen.GetPublishedSiteRequestObject) (gen.GetPublishedSiteResponseObject, error) {
	host, err := contract.NormalizeHost(req.Params.Host)
	if err != nil {
		return nil, apperrors.New(400, "invalid_request", "Invalid DNS hostname")
	}
	problem := gen.Problem{Type: "about:blank", Title: "Published read unavailable", Status: 503, Code: "unavailable"}
	if s.reader == nil {
		return gen.GetPublishedSite503ApplicationProblemPlusJSONResponse(problem), nil
	}
	site, err := s.reader.Current(ctx, host)
	if errors.Is(err, contract.ErrNotFound) {
		problem.Title = "Not Found"
		problem.Status = 404
		problem.Code = "not_found"
		return gen.GetPublishedSite404ApplicationProblemPlusJSONResponse(problem), nil
	}
	if err != nil {
		return gen.GetPublishedSite503ApplicationProblemPlusJSONResponse(problem), nil
	}
	// Both DTOs are schema-owned values; conversion keeps serialization on the strict boundary.
	raw, err := json.Marshal(site)
	if err != nil {
		return nil, err
	}
	var dto gen.PublishedSite
	if err = json.Unmarshal(raw, &dto); err != nil {
		return nil, err
	}
	return gen.GetPublishedSite200JSONResponse(dto), nil
}
