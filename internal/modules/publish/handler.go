// Responsibility: anonymous strict generated Read Context HTTP transport.
// Dependencies: publish public contract and platform strict transport; caller: cmd/api bootstrap.
// Invariants: exactly one validated host, bounded read deadline, no forwarded-host trust;
// 200 bodies are the stored serving bytes, never re-encoded.
package publish

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"net/http"
	"net/url"
	"time"
)

type storefrontHandler struct {
	httpapi.StrictServerInterface
	reader contract.Reader
}

func StorefrontRoute(reader contract.Reader) httpapi.Route {
	s := &storefrontHandler{reader: reader}
	spec := httpapi.RouteSpec{OperationID: "getPublishedSite", Method: "GET", Pattern: "/api/v1/public/sites/current", Access: httpapi.Public}
	strict := httpapi.StrictRoutes(s, []httpapi.RouteSpec{spec})[0].Handler
	return httpapi.Route{Spec: spec, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(query) != 1 || len(query["host"]) != 1 {
			httpapi.WriteProblem(w, r, apperrors.New(400, "invalid_request", "Exactly one host is required"))
			return
		}
		host, err := contract.NormalizeHost(query.Get("host"))
		if err != nil {
			httpapi.WriteProblem(w, r, apperrors.New(400, "invalid_request", "Invalid DNS hostname"))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		// 200 bodies are the stored serving bytes, written as-is (no re-encoding); errors
		// keep the strict transport's problem responses.
		if reader == nil {
			strict.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		body, err := reader.Serve(ctx, host)
		if err != nil {
			strict.ServeHTTP(w, r.WithContext(context.WithValue(ctx, serveErrKey{}, err)))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})}
}
func (s *storefrontHandler) GetPublishedSite(ctx context.Context, req httpapi.GetPublishedSiteRequestObject) (httpapi.GetPublishedSiteResponseObject, error) {
	host, err := contract.NormalizeHost(req.Params.Host)
	if err != nil {
		return nil, apperrors.New(400, "invalid_request", "Invalid DNS hostname")
	}
	problem := httpapi.Problem{Type: "about:blank", Title: "Published read unavailable", Status: 503, Code: "unavailable"}
	if s.reader == nil {
		return httpapi.GetPublishedSite503ApplicationProblemPlusJSONResponse(problem), nil
	}
	err, _ = ctx.Value(serveErrKey{}).(error)
	var body []byte
	if err == nil {
		body, err = s.reader.Serve(ctx, host)
	}
	if errors.Is(err, contract.ErrNotFound) {
		problem.Title = "Not Found"
		problem.Status = 404
		problem.Code = "not_found"
		return httpapi.GetPublishedSite404ApplicationProblemPlusJSONResponse(problem), nil
	}
	if err != nil {
		return httpapi.GetPublishedSite503ApplicationProblemPlusJSONResponse(problem), nil
	}
	var dto httpapi.PublishedSite
	if err = json.Unmarshal(body, &dto); err != nil {
		return nil, err
	}
	return httpapi.GetPublishedSite200JSONResponse(dto), nil
}

// serveErrKey carries the read error into the strict transport so problem bodies stay generated.
type serveErrKey struct{}
