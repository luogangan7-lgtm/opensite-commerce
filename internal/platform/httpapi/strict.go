// Responsibilities: mount only module-owned generated operations and validate
// bounded strict JSON before generated decoding. Dependencies: generated strict
// std-http wrappers, embedded OAS and shared admission; callers: module Routes.
// Invariants: authentication runs in the outer router first; planned operations
// never become reachable through the generated mux; startup mismatches fail.
package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
)

// StrictRoutes is static composition. Invalid embedded contracts panic and are
// recovered by bootstrap.New as startup errors, rather than a serving fallback.
func StrictRoutes(service generated.StrictServerInterface, specs []RouteSpec) []Route {
	doc, err := GetSwagger()
	if err != nil {
		panic(fmt.Errorf("load API contract: %w", err))
	}
	collector := &strictRouteCollector{handlers: make(map[string]http.HandlerFunc), wanted: make(map[string]bool)}
	for _, spec := range specs {
		collector.wanted[spec.Method+" "+spec.Pattern] = true
	}
	requestError := func(w http.ResponseWriter, r *http.Request, _ error) {
		WriteProblem(w, r, apperrors.New(400, "invalid_request", "Request does not conform to the API contract"))
	}
	strict := generated.NewStrictHandlerWithOptions(service, nil, generated.StrictHTTPServerOptions{RequestErrorHandlerFunc: requestError, ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) { WriteProblem(w, r, err) }})
	generated.HandlerWithOptions(strict, generated.StdHTTPServerOptions{BaseRouter: collector, ErrorHandlerFunc: requestError})
	routes := make([]Route, 0, len(specs))
	for _, spec := range specs {
		item := doc.Paths.Value(spec.Pattern)
		if item == nil {
			panic("missing API path: " + spec.Pattern)
		}
		op := item.GetOperation(spec.Method)
		// Codegen normalizes the embedded operation ID into its Go method name.
		if op == nil || spec.OperationID == "" || op.OperationID != strings.ToUpper(spec.OperationID[:1])+spec.OperationID[1:] || op.Extensions["x-opensite-status"] != "implemented" {
			panic("missing implemented API operation: " + spec.OperationID)
		}
		scope, _ := op.Extensions["x-required-scope"].(string)
		if op.Extensions["x-opensite-access"] != string(spec.Access) || scope != spec.Scope {
			panic("API authorization contract mismatch: " + spec.OperationID)
		}
		handler := collector.handlers[spec.Method+" "+spec.Pattern]
		if handler == nil {
			panic("missing generated operation: " + spec.OperationID)
		}
		var schema *openapi3.Schema
		binary := false
		if op.RequestBody != nil && op.RequestBody.Value != nil {
			binary = op.RequestBody.Value.Content["application/octet-stream"] != nil
			if media := op.RequestBody.Value.Content["application/json"]; media != nil && media.Schema != nil {
				schema = media.Schema.Value
			}
		}
		routes = append(routes, Route{Spec: spec, Handler: strictAdmission(handler, schema, binary, requiresVersion(op), op.Extensions["x-opensite-if-match-zero"] == true, op.Parameters)})
	}
	return routes
}

// This collector captures wrapper functions without mounting a generated mux.
// Its only reachable handlers are returned to the authenticated outer router.
type strictRouteCollector struct {
	handlers map[string]http.HandlerFunc
	wanted   map[string]bool
}

func (c *strictRouteCollector) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	if c.wanted[pattern] {
		c.handlers[pattern] = handler
	}
}
func (c *strictRouteCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	WriteProblem(w, r, apperrors.New(404, "not_found", "Route not found"))
}

func requiresVersion(op *openapi3.Operation) bool {
	for _, param := range op.Parameters {
		if param.Value != nil && param.Value.In == "header" && strings.EqualFold(param.Value.Name, "If-Match") && param.Value.Required {
			return true
		}
	}
	return false
}
func strictAdmission(next http.Handler, schema *openapi3.Schema, binary, version, allowZero bool, parameters openapi3.Parameters) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, parameter := range parameters {
			if parameter.Value != nil && parameter.Value.In == "header" && strings.EqualFold(parameter.Value.Name, "Idempotency-Key") {
				if _, err := idempotency.ValidateKey(r.Header); err != nil {
					WriteProblem(w, r, err)
					return
				}
			}
		}
		if version {
			if err := admitVersion(r, allowZero); err != nil {
				WriteProblem(w, r, err)
				return
			}
		}
		input := &openapi3filter.RequestValidationInput{Request: r, Options: &openapi3filter.Options{SkipSettingDefaults: true}}
		for _, parameter := range parameters {
			if parameter.Value == nil || (parameter.Value.In != "query" && parameter.Value.In != "header") {
				continue
			}
			values := r.URL.Query()[parameter.Value.Name]
			if parameter.Value.In == "header" {
				values = r.Header.Values(parameter.Value.Name)
			}
			if len(values) > 1 || openapi3filter.ValidateParameter(r.Context(), input, parameter.Value) != nil {
				WriteProblem(w, r, apperrors.New(400, "invalid_request", "Parameters do not conform to the API contract"))
				return
			}
		}
		if binary {
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/octet-stream" {
				WriteProblem(w, r, apperrors.New(415, "unsupported_media_type", "Content-Type must be application/octet-stream"))
				return
			}
			if r.ContentLength > maxBodySize(r) {
				WriteProblem(w, r, apperrors.New(413, "request_too_large", "Request body exceeds the route limit"))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBodySize(r))
		}
		if schema != nil {
			var raw json.RawMessage
			if err := DecodeJSON(w, r, &raw); err != nil {
				WriteProblem(w, r, err)
				return
			}
			var value any
			if json.Unmarshal(raw, &value) != nil || schema.VisitJSON(value) != nil {
				WriteProblem(w, r, invalidJSON())
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		next.ServeHTTP(w, r)
	})
}
func admitVersion(r *http.Request, allowZero bool) error {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		return apperrors.New(428, "precondition_required", "If-Match is required")
	}
	invalid := func() error {
		return apperrors.New(400, "invalid_if_match", "If-Match must be a quoted positive decimal version")
	}
	if len(values) != 1 {
		return invalid()
	}
	value := values[0]
	if len(value) < 3 || value[0] != '"' || value[len(value)-1] != '"' {
		return invalid()
	}
	if allowZero && value == `"0"` {
		return nil
	}
	digits := value[1 : len(value)-1]
	if digits[0] == '0' || strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return invalid()
	}
	if n, err := strconv.ParseInt(digits, 10, 64); err != nil || n < 1 {
		return invalid()
	}
	return nil
}
