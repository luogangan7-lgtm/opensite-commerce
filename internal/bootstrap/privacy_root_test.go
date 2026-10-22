// Responsibilities: reject divergent privacy storage roots before DB access.
// Dependencies: environment composition; gates prove the shared private root.
package bootstrap_test

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"strings"
	"testing"
)

func TestPrivacyRootMismatchFailsBeforeDatabase(t *testing.T) {
	for _, name := range []string{"OPENSITE_DATABASE_URL", "OPENSITE_MEDIA_DATABASE_URL", "OPENSITE_RESPONSE_ROOT"} {
		t.Setenv(name, "")
	}
	// Root validation precedes every capability, even a malformed runtime DSN.
	t.Setenv("OPENSITE_DATABASE_URL", "invalid-runtime-synthetic-dsn")
	t.Setenv("OPENSITE_MEDIA_ROOT", t.TempDir())
	t.Setenv("OPENSITE_PRIVACY_ROOT", t.TempDir())
	t.Setenv("OPENSITE_PRIVACY_DATABASE_URL", "invalid-synthetic-dsn")
	_, _, err := bootstrap.FromEnvironment(context.Background(), bootstrap.Config{})
	if err == nil || !strings.Contains(err.Error(), "privacy root must equal") {
		t.Fatalf("root mismatch not refused before connection: %v", err)
	}
}
