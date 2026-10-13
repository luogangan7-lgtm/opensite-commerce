// Responsibility: boundary errors redact DB detail and preserve stable statuses.
// Dependencies: pgx error values/standard library; no SQL semantic mock claims.
package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"strings"
	"testing"
	"time"
)

func TestMapErrorSafeStatuses(t *testing.T) {
	for code, want := range map[string]int{"P0002": 404, "42501": 404, "55000": 409, "22023": 400, "XX000": 503} {
		e := MapError(&pgconn.PgError{Code: code, Message: "secret DB detail"})
		var app *apperrors.Error
		if !errors.As(e, &app) || app.Status != want || strings.Contains(e.Error(), "secret") {
			t.Fatal("NOTIFICATION_ERROR_BOUNDARY", e, want)
		}
	}
}

func TestMapErrorPreservesCancellationClassification(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled} {
		for _, input := range []error{cause, fmt.Errorf("secret driver detail: %w", cause)} {
			mapped := MapError(input)
			if !errors.Is(mapped, cause) {
				t.Fatal("NOTIFICATION_CONTEXT_CLASSIFICATION", mapped)
			}
			if mapped.Error() != cause.Error() {
				t.Fatal("NOTIFICATION_CONTEXT_REDACTION", mapped)
			}
		}
	}
	mapped := MapError(&pgconn.PgError{Code: "55000", Message: "secret SQL detail"})
	var app *apperrors.Error
	if !errors.As(mapped, &app) || app.Status != 409 || errors.Is(mapped, context.DeadlineExceeded) || errors.Is(mapped, context.Canceled) {
		t.Fatal("NOTIFICATION_SQL_NOT_CONTEXT", mapped)
	}
}

type failedBeginQueue struct {
	pgxadapter.QueueDB
	cause error
	wait  bool
}

func (q failedBeginQueue) Begin(ctx context.Context) (pgx.Tx, error) {
	if q.wait {
		<-ctx.Done()
	}
	if q.cause == nil {
		return nil, fmt.Errorf("private driver detail: %w", ctx.Err())
	}
	return nil, q.cause
}
func TestClaimPreservesWrappedDeadlineClassification(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled} {
		w := Worker{Pool: failedBeginQueue{cause: fmt.Errorf("private driver detail: %w", cause)}}
		rows, e := w.Claim(context.Background(), "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", 1, 30)
		if rows != nil || !errors.Is(e, cause) {
			t.Fatal("NOTIFICATION_CLAIM_CONTEXT_CLASSIFICATION", e)
		}
		if e.Error() != cause.Error() {
			t.Fatal("NOTIFICATION_CLAIM_CONTEXT_REDACTION", e)
		}
	}
	w := Worker{Pool: failedBeginQueue{cause: &pgconn.PgError{Code: "55000", Message: "private SQL detail"}}}
	_, e := w.Claim(context.Background(), "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", 1, 30)
	var app *apperrors.Error
	if !errors.As(e, &app) || app.Status != 409 || errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("NOTIFICATION_CLAIM_SQL_NOT_CONTEXT", e)
	}
}

func TestClaimNaturalDeadlineAndGenuineFailure(t *testing.T) {
	for _, genuine := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		q := failedBeginQueue{wait: true}
		if genuine {
			q.cause = &pgconn.PgError{Code: "XX000", Message: "private genuine SQL failure"}
		}
		_, e := (Worker{Pool: q}).Claim(ctx, "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", 1, 30)
		cancel()
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatal("NOTIFICATION_NATURAL_CLAIM_BUDGET")
		}
		if !genuine {
			if !errors.Is(e, context.DeadlineExceeded) || e.Error() != context.DeadlineExceeded.Error() {
				t.Fatal("NOTIFICATION_NATURAL_CLAIM_CLASSIFICATION", e)
			}
		} else {
			var app *apperrors.Error
			if !errors.As(e, &app) || app.Status != 503 || errors.Is(e, context.DeadlineExceeded) {
				t.Fatal("NOTIFICATION_GENUINE_ERROR_FATAL", e)
			}
		}
	}
}
