// Responsibilities: bind retryable erasure contention to a stable public problem.
// Dependencies: domain errors and HTTP problem envelope, no DB or raw diagnostics.
package datarightshttp

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func TestPrivacyBusyProblem(t *testing.T) {
	err := privacyError(fmt.Errorf("private diagnostic: %w", contract.ErrBusy))
	w := httptest.NewRecorder()
	httpapi.WriteProblem(w, httptest.NewRequest("POST", "/", nil), err)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"privacy_busy"`) || strings.Contains(w.Body.String(), "private diagnostic") {
		t.Fatalf("busy response %d %s", w.Code, w.Body.String())
	}
}
