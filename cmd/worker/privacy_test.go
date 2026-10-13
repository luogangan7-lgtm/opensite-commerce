//go:build pgx

// Responsibilities: reject divergent private storage at worker startup before
// opening credentials. Dependencies: production composeWorkers; Go gates call it.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	rights "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
	"github.com/riverqueue/river"
)

func TestPrivacyWorkerRootMismatchBeforeDatabase(t *testing.T) {
	values := map[string]string{"PRIVACY_WORKER_DATABASE_URL": "invalid-synthetic-dsn", "OPENSITE_MEDIA_ROOT": t.TempDir(), "OPENSITE_PRIVACY_ROOT": t.TempDir()}
	_, _, _, err := composeWorkers(context.Background(), func(k string) string { return values[k] }, nil)
	if err == nil || !strings.Contains(err.Error(), "privacy root must equal") {
		t.Fatalf("root mismatch not refused: %v", err)
	}
}

type exportResult struct{ err error }

func (e exportResult) BuildExport(context.Context, string, string) error { return e.err }

// Permanent contract errors cancel instead of retrying; transient ones retry (thread T8).
func TestPermanentExportErrorsCancel(t *testing.T) {
	var cancel *river.JobCancelError
	for _, err := range []error{rights.ErrInvalid, rights.ErrConflict, rights.ErrNotFound, fmt.Errorf("wrapped: %w", rights.ErrConflict)} {
		if got := (permanentExports{exportResult{err}}).BuildExport(context.Background(), "t", "r"); !errors.As(got, &cancel) || !errors.Is(got, err) {
			t.Fatalf("%v not cancelled: %v", err, got)
		}
	}
	for _, err := range []error{nil, rights.ErrBusy, rights.ErrUnavailable} {
		if got := (permanentExports{exportResult{err}}).BuildExport(context.Background(), "t", "r"); errors.As(got, &cancel) || !errors.Is(got, err) {
			t.Fatalf("%v cancelled or changed: %v", err, got)
		}
	}
}
