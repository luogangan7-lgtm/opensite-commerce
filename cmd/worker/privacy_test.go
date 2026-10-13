//go:build pgx

// Responsibilities: reject divergent private storage at worker startup before
// opening credentials. Dependencies: production composeWorkers; Go gates call it.
package main

import (
	"context"
	"strings"
	"testing"
)

func TestPrivacyWorkerRootMismatchBeforeDatabase(t *testing.T) {
	values := map[string]string{"PRIVACY_WORKER_DATABASE_URL": "invalid-synthetic-dsn", "OPENSITE_MEDIA_ROOT": t.TempDir(), "OPENSITE_PRIVACY_ROOT": t.TempDir()}
	_, _, _, err := composeWorkers(context.Background(), func(k string) string { return values[k] }, nil)
	if err == nil || !strings.Contains(err.Error(), "privacy root must equal") {
		t.Fatalf("root mismatch not refused: %v", err)
	}
}
