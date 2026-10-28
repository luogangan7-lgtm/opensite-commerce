// Responsibilities: public-API strict JSON and RouteSpec size-limit regression gate.
// Dependencies: stdlib testing/HTTP/JSON; httpapi public boundary; no database.
// Callers: go test; invariants: independent literal statuses/codes and boundaries.
package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type jsonDTO struct {
	Name string          `json:"name"`
	Meta json.RawMessage `json:"meta"`
}

func TestDecodeJSONAdmission(t *testing.T) {
	tests := []struct {
		name, contentType, body string
		status                  int
		code                    string
	}{
		{"valid", "application/json", `{"name":"ok"}`, 204, ""},
		{"charset", "application/json; charset=utf-8", `{"name":"ok"}`, 204, ""},
		{"case insensitive media type", "Application/JSON", `{"name":"ok"}`, 204, ""},
		{"missing media type", "", `{}`, 415, "unsupported_media_type"},
		{"wrong media type", "text/plain", `{}`, 415, "unsupported_media_type"},
		{"suffix media type", "application/merge-patch+json", `{}`, 415, "unsupported_media_type"},
		{"malformed media type", "application/json; charset", `{}`, 415, "unsupported_media_type"},
		{"unknown field", "application/json", `{"name":"ok","secret":true}`, 400, "invalid_json"},
		{"type error", "application/json", `{"name":42}`, 400, "invalid_json"},
		{"empty", "application/json", ``, 400, "invalid_json"},
		{"whitespace", "application/json", " \n\t ", 400, "invalid_json"},
		{"malformed", "application/json", `{"name":`, 400, "invalid_json"},
		{"null root", "application/json", `null`, 400, "invalid_json"},
		{"array root", "application/json", `[]`, 400, "invalid_json"},
		{"scalar root", "application/json", `true`, 400, "invalid_json"},
		{"second object", "application/json", `{} {}`, 400, "invalid_json"},
		{"second null", "application/json", `{} null`, 400, "invalid_json"},
		{"trailing junk", "application/json", `{} !`, 400, "invalid_json"},
		{"trailing whitespace", "application/json", "{} \n\t", 204, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, code, dto := decodeJSONRequest(t, tt.contentType, tt.body, 0)
			assertJSONOutcome(t, status, code, tt.status, tt.code)
			if tt.status == 204 && strings.Contains(tt.body, "ok") && dto.Name != "ok" {
				t.Fatalf("decoded name=%q", dto.Name)
			}
		})
	}
}

func TestDecodeJSONRouteBodyLimit(t *testing.T) {
	tests := []struct {
		name, body string
		limit      int64
		status     int
		code       string
	}{
		{"exact route limit", `{"name":"ok"}`, 13, 204, ""},
		{"one byte above route limit", `{"name":"ok"}`, 12, 413, "request_too_large"},
		{"oversize whitespace suffix", `{}` + strings.Repeat(" ", 15), 16, 413, "request_too_large"},
		{"default exact 1MiB", `{}` + strings.Repeat(" ", (1<<20)-2), 0, 204, ""},
		{"default above 1MiB", `{}` + strings.Repeat(" ", (1<<20)-1), 0, 413, "request_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, code, _ := decodeJSONRequest(t, "application/json", tt.body, tt.limit)
			assertJSONOutcome(t, status, code, tt.status, tt.code)
		})
	}
}

func TestDecodeJSONDepthAndStringLimits(t *testing.T) {
	// RawMessage exercises limits before opaque application payload decoding.
	tests := []struct {
		name, body string
		status     int
	}{
		{"root plus 63 arrays", `{"meta":` + strings.Repeat("[", 63) + `0` + strings.Repeat("]", 63) + `}`, 204},
		{"root plus 64 arrays", `{"meta":` + strings.Repeat("[", 64) + `0` + strings.Repeat("]", 64) + `}`, 400},
		{"mixed depth above limit", `{"meta":` + strings.Repeat(`{"x":[`, 32) + `0` + strings.Repeat(`]}`, 32) + `}`, 400},
		{"value exact 65536 bytes", `{"name":"` + strings.Repeat("a", 65536) + `"}`, 204},
		{"value above 65536 bytes", `{"name":"` + strings.Repeat("a", 65537) + `"}`, 400},
		{"escaped value exact decoded bytes", `{"name":"` + strings.Repeat(`\u0061`, 65536) + `"}`, 204},
		{"escaped value above decoded bytes", `{"name":"` + strings.Repeat(`\u0061`, 65537) + `"}`, 400},
		{"multibyte value exact decoded bytes", `{"name":"` + strings.Repeat("é", 32768) + `"}`, 204},
		{"multibyte value above decoded bytes", `{"name":"` + strings.Repeat("é", 32769) + `"}`, 400},
		{"opaque key exact bytes", `{"meta":{"` + strings.Repeat("k", 65536) + `":0}}`, 204},
		{"opaque key above bytes", `{"meta":{"` + strings.Repeat("k", 65537) + `":0}}`, 400},
		{"escaped opaque key above decoded bytes", `{"meta":{"` + strings.Repeat(`\u006b`, 65537) + `":0}}`, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, code, _ := decodeJSONRequest(t, "application/json", tt.body, 0)
			wantCode := ""
			if tt.status == 400 {
				wantCode = "invalid_json"
			}
			assertJSONOutcome(t, status, code, tt.status, wantCode)
		})
	}
}

func TestDecodeJSONRejectsUnknownNestedDTOFields(t *testing.T) {
	var dto struct {
		Child struct {
			Name string `json:"name"`
		} `json:"child"`
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"child":{"name":"ok","extra":true}}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	err := httpapi.DecodeJSON(w, r, &dto)
	if err == nil {
		t.Fatal("unknown nested field was accepted")
	}
	httpapi.WriteProblem(w, r, err)
	assertJSONOutcome(t, w.Code, problemCode(t, w), 400, "invalid_json")
}

func TestDecodeJSONDuplicateObjectMembers(t *testing.T) {
	tests := []struct {
		name, body string
		reject     bool
	}{
		{"root", `{"name":"updated","name":"again"}`, true},
		{"escaped root key", `{"name":"updated","\u006eame":"again"}`, true},
		{"nested opaque object", `{"name":"updated","meta":{"secret":1,"secret":2}}`, true},
		{"deep nested object", `{"name":"updated","meta":{"outer":{"secret":1,"secret":2}}}`, true},
		{"object in array", `{"name":"updated","meta":[{"secret":1,"secret":2}]}`, true},
		{"object in nested arrays", `{"name":"updated","meta":[[0,{"secret":1,"secret":2}]]}`, true},
		{"escaped nested key", `{"name":"updated","meta":{"secret":1,"\u0073ecret":2}}`, true},
		{"escaped supplementary key", `{"name":"updated","meta":{"😀":1,"\ud83d\ude00":2}}`, true},
		{"empty key", `{"name":"updated","meta":{"":1,"":2}}`, true},
		{"after string", `{"name":"updated","meta":{"secret":"secret","secret":2}}`, true},
		{"after boolean", `{"name":"updated","meta":{"secret":true,"secret":2}}`, true},
		{"after null", `{"name":"updated","meta":{"secret":null,"secret":2}}`, true},
		{"after object", `{"name":"updated","meta":{"secret":{},"secret":2}}`, true},
		{"after array", `{"name":"updated","meta":{"secret":[],"secret":2}}`, true},
		{"sibling objects", `{"name":"updated","meta":{"left":{"same":1},"right":{"same":2}}}`, false},
		{"array sibling objects", `{"name":"updated","meta":[{"same":1},{"same":2}]}`, false},
		{"parent and child same name", `{"name":"updated","meta":{"same":{"same":1}}}`, false},
		{"repeated string values", `{"name":"updated","meta":{"left":"same","right":"same","same":"same"}}`, false},
		{"array strings equal keys", `{"name":"updated","meta":{"same":["same","same"],"next":"same"}}`, false},
		{"distinct decoded keys", `{"name":"updated","meta":{"same":1,"Same":2,"é":3,"e\u0301":4}}`, false},
		{"all value types followed by keys", `{"name":"updated","meta":{"s":"s","n":0,"b":false,"nil":null,"o":{},"a":[],"last":1}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dto := jsonDTO{Name: "initial", Meta: json.RawMessage(`{"initial":true}`)}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			err := httpapi.DecodeJSON(w, r, &dto)
			if !tt.reject {
				if err != nil || dto.Name != "updated" {
					t.Fatalf("valid object: err=%v; name=%q", err, dto.Name)
				}
				return
			}
			if err == nil {
				t.Fatal("duplicate object member was accepted")
			}
			assertDuplicateJSONProblem(t, w, r, err)
			if dto.Name != "initial" || string(dto.Meta) != `{"initial":true}` {
				t.Fatalf("destination changed before validation failed: %+v", dto)
			}
		})
	}
}

func TestDecodeJSONDuplicateBeforeCustomUnmarshal(t *testing.T) {
	var dto struct {
		Name string            `json:"name"`
		Meta jsonCustomPayload `json:"meta"`
	}
	dto.Name, dto.Meta.Calls = "initial", 7
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"name":"updated","meta":{"secret":1,"secret":2}}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	err := httpapi.DecodeJSON(w, r, &dto)
	if err == nil {
		t.Fatal("duplicate object member reached custom unmarshal")
	}
	assertDuplicateJSONProblem(t, w, r, err)
	if dto.Name != "initial" || dto.Meta.Calls != 7 {
		t.Fatalf("destination/custom unmarshal changed before validation failed: %+v", dto)
	}
}

type jsonCustomPayload struct{ Calls int }

func (payload *jsonCustomPayload) UnmarshalJSON([]byte) error {
	payload.Calls++
	return nil
}

func assertDuplicateJSONProblem(t *testing.T, w *httptest.ResponseRecorder, r *http.Request, err error) {
	t.Helper()
	httpapi.WriteProblem(w, r, err)
	assertJSONOutcome(t, w.Code, problemCode(t, w), 400, "invalid_json")
	if contentType := w.Header().Get("Content-Type"); contentType != "application/problem+json" {
		t.Fatalf("content type=%q", contentType)
	}
	var problem struct {
		Type, Title, Detail, Instance string
		Status                        int
	}
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Type != "about:blank" || problem.Title != "Bad Request" || problem.Status != 400 ||
		problem.Detail != "Body must be one valid JSON object within the JSON limits" || problem.Instance != "/api/v1/auth/login" {
		t.Fatalf("unexpected public problem: %+v", problem)
	}
}

func TestDecodeJSONBodyReadFailure(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	r.Header.Set("Content-Type", "application/json")
	r.Body = io.NopCloser(jsonFailingReader{})
	w := httptest.NewRecorder()
	var dto jsonDTO
	err := httpapi.DecodeJSON(w, r, &dto)
	if err == nil {
		t.Fatal("body read failure was accepted")
	}
	httpapi.WriteProblem(w, r, err)
	assertJSONOutcome(t, w.Code, problemCode(t, w), 400, "invalid_json")
}

type jsonFailingReader struct{}

func (jsonFailingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func decodeJSONRequest(t *testing.T, contentType, body string, limit int64) (int, string, jsonDTO) {
	t.Helper()
	router := httpapi.New(nil, nil)
	var dto jsonDTO
	err := router.Register(httpapi.RouteSpec{
		OperationID: "decode", Method: http.MethodPost, Pattern: "/api/v1/auth/login", Access: httpapi.Public, MaxBodySize: limit,
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := httpapi.DecodeJSON(w, r, &dto); err != nil {
			httpapi.WriteProblem(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	// MaxBytesReader must enforce bytes read, independent of Content-Length.
	r.ContentLength = -1
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	code := ""
	if w.Code != http.StatusNoContent {
		code = problemCode(t, w)
	}
	return w.Code, code, dto
}

func problemCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatalf("problem decode: %v; body=%s", err, w.Body.String())
	}
	return problem.Code
}

func assertJSONOutcome(t *testing.T, status int, code string, wantStatus int, wantCode string) {
	t.Helper()
	if status != wantStatus || code != wantCode {
		t.Fatalf("got status/code %d/%q; want %d/%q", status, code, wantStatus, wantCode)
	}
}
