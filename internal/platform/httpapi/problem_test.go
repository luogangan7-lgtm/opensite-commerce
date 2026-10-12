// Problem contract tests call only public transport/domain APIs. Stdlib
// httptest/JSON observe the response; no tables or internals are inspected.
// Independent expectations guard status mapping, wrapping and information leaks.
package httpapi

import (
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	platformerrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
)

func TestWriteProblem(t *testing.T) {
	cases := []struct {
		name                string
		err                 error
		status              int
		title, code, detail string
	}{
		{"bad_request", platformerrors.New(400, "", "bad input"), 400, "Bad Request", "invalid_request", "bad input"},
		{"unauthenticated", platformerrors.New(401, "", "login required"), 401, "Unauthorized", "unauthenticated", "login required"},
		{"forbidden", platformerrors.New(403, "", "scope denied"), 403, "Forbidden", "forbidden", "scope denied"},
		{"not_found", platformerrors.New(404, "", "missing"), 404, "Not Found", "not_found", "missing"},
		{"conflict", platformerrors.New(409, "", "duplicate"), 409, "Conflict", "conflict", "duplicate"},
		{"precondition", platformerrors.New(412, "", "etag mismatch"), 412, "Precondition Failed", "precondition_failed", "etag mismatch"},
		{"large", platformerrors.New(413, "", "too large"), 413, "Request Entity Too Large", "payload_too_large", "too large"},
		{"media", platformerrors.New(415, "", "bad media"), 415, "Unsupported Media Type", "unsupported_media_type", "bad media"},
		{"validation", platformerrors.New(422, "", "invalid"), 422, "Unprocessable Entity", "validation_failed", "invalid"},
		{"required", platformerrors.New(428, "", "etag required"), 428, "Precondition Required", "precondition_required", "etag required"},
		{"rate", platformerrors.New(429, "", "slow down"), 429, "Too Many Requests", "rate_limited", "slow down"},
		{"internal", platformerrors.New(500, "secret_database_code", "SECRET database failure"), 500, "Internal Server Error", "internal_error", ""},
		{"unavailable", platformerrors.New(503, "SECRET database code", "SECRET database failure"), 503, "Service Unavailable", "service_unavailable", ""},
		{"custom", platformerrors.New(422, "invalid_sku", "invalid sku"), 422, "Unprocessable Entity", "invalid_sku", "invalid sku"},
		{"wrapped", fmt.Errorf("SECRET wrapper: %w", platformerrors.New(403, "missing_scope", "denied")), 403, "Forbidden", "missing_scope", "denied"},
		{"unknown", stderrors.New("SECRET connection string"), 500, "Internal Server Error", "internal_error", ""},
		{"nil", nil, 500, "Internal Server Error", "internal_error", ""},
		{"typed_nil", (*platformerrors.Error)(nil), 500, "Internal Server Error", "internal_error", ""},
	}
	for _, invalid := range []int{-1, 0, 200, 302, 402, 499, 501, 600} {
		cases = append(cases, struct {
			name                string
			err                 error
			status              int
			title, code, detail string
		}{
			fmt.Sprintf("invalid_%d", invalid), platformerrors.New(invalid, "SECRET invalid code", "SECRET internal detail"), 500, "Internal Server Error", "internal_error", "",
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "https://example.test/api/items?token=SECRET", nil)
			w := httptest.NewRecorder()
			WriteProblem(w, r, tc.err)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d", w.Code, tc.status)
			}
			if got := w.Header().Get("Content-Type"); got != "application/problem+json" {
				t.Fatalf("content-type=%q", got)
			}
			var got map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"type": "about:blank", "title": tc.title, "status": float64(tc.status), "code": tc.code, "detail": tc.detail, "instance": "/api/items", "request_id": ""}
			if len(got) != len(want) {
				t.Fatalf("unexpected fields: %v", got)
			}
			for key, value := range want {
				if got[key] != value {
					t.Errorf("%s=%v want=%v", key, got[key], value)
				}
			}
			if strings.Contains(w.Body.String(), "SECRET") {
				t.Errorf("internal error or query leaked: %s", w.Body.String())
			}
		})
	}
}

func TestDomainErrorPublicAPI(t *testing.T) {
	failure := platformerrors.New(409, "duplicate_order", "duplicate")
	if failure.Status != 409 || failure.Code != "duplicate_order" || failure.Detail != "duplicate" {
		t.Fatalf("fields=%+v", failure)
	}
	if failure.Error() != "duplicate" {
		t.Fatalf("Error()=%q", failure.Error())
	}
	var standard error = failure
	var recovered *platformerrors.Error
	if !stderrors.As(fmt.Errorf("wrapped: %w", standard), &recovered) || recovered != failure {
		t.Fatal("standard wrapping did not preserve domain error")
	}
}
