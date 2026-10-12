// HTTP problem mapping owns the public RFC 9457 error boundary. It depends on
// stdlib JSON/HTTP/errors, platform/errors and RequestID; handlers and recovery
// call WriteProblem. No tables are touched; 5xx details and query strings never
// enter responses.
package httpapi

import (
	"encoding/json"
	stderrors "errors"
	"net/http"

	platformerrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
)

// WriteProblem publishes a domain failure or a redacted internal error.
func WriteProblem(w http.ResponseWriter, r *http.Request, err error) {
	status, code, detail := http.StatusInternalServerError, "internal_error", ""
	var failure *platformerrors.Error
	if stderrors.As(err, &failure) && failure != nil {
		if defaultCode, ok := problemCode(failure.Status); ok && failure.Status < 500 {
			status, code, detail = failure.Status, defaultCode, failure.Detail
			if failure.Code != "" {
				code = failure.Code
			}
		}
	}
	body := struct {
		Type      string `json:"type"`
		Title     string `json:"title"`
		Status    int    `json:"status"`
		Code      string `json:"code"`
		Detail    string `json:"detail"`
		Instance  string `json:"instance"`
		RequestID string `json:"request_id"`
	}{"about:blank", http.StatusText(status), status, code, detail, r.URL.Path, RequestID(r.Context())}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func problemCode(status int) (string, bool) {
	switch status {
	case 400:
		return "invalid_request", true
	case 401:
		return "unauthenticated", true
	case 403:
		return "forbidden", true
	case 404:
		return "not_found", true
	case 409:
		return "conflict", true
	case 412:
		return "precondition_failed", true
	case 413:
		return "payload_too_large", true
	case 415:
		return "unsupported_media_type", true
	case 422:
		return "validation_failed", true
	case 428:
		return "precondition_required", true
	case 429:
		return "rate_limited", true
	case 500:
		return "internal_error", true
	default:
		return "", false
	}
}
