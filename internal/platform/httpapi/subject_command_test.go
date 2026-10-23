// Responsibility: command body admission before persistence or typed decoding.
// Dependencies: net/http bounded readers; caller: foundation Go gate.
// Invariant: reader size failures are 413, other read failures remain 400.
package httpapi

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type commandReadError struct{ err error }

func (r commandReadError) Read([]byte) (int, error) { return 0, r.err }

func TestSubjectCommandReadErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{&http.MaxBytesError{Limit: 8}, 413, "request_too_large"},
		{errors.New("broken reader"), 400, "invalid_request"},
	} {
		route := AdmitCommand(Route{Spec: RouteSpec{MaxBodySize: 8}, Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("failed body reached handler") })})
		req := httptest.NewRequest("POST", "/", io.NopCloser(commandReadError{tc.err}))
		req.Header.Set("Idempotency-Key", "subject-size-test")
		rr := httptest.NewRecorder()
		route.Handler.ServeHTTP(rr, req)
		if rr.Code != tc.status || !strings.Contains(rr.Body.String(), `"code":"`+tc.code+`"`) {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
}
