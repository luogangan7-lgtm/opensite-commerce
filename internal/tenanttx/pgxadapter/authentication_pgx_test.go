//go:build pgx

// Responsibilities: prove a live maintenance LOGIN cannot enter staff authentication work.
// Dependencies: CI disposable PostgreSQL and private adapter; callers: tagged CI suites.
// Invariants: exact app_auth authority is checked before the SQL-query callback is exposed.
package pgxadapter_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestAuthenticationPortRejectsMaintenanceLoginPostgres(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: disposable maintenance LOGIN fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runner, err := pgxadapter.OpenAuthorization(ctx, dsn)
	if err != nil {
		t.Fatal("maintenance fixture unavailable")
	}
	defer func() { _ = runner.Close(context.Background()) }()
	// Establish that the exact existing role predicate is executable and rejects
	// this fixture, so an absent schema cannot masquerade as the negative witness.
	if err := runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		valid, err := db.New(tx).ValidateAuthConnection(ctx)
		if err != nil {
			return err
		}
		if valid.Valid && valid.Bool {
			t.Fatal("fixture must have maintenance authority rather than app_auth")
		}
		return nil
	}); err != nil {
		t.Fatal("exact authentication role predicate unavailable")
	}
	called := false
	if err := authenticationPort(t, runner).RunAuthentication(ctx, func(context.Context, *db.Queries) error {
		called = true
		return nil
	}); err == nil || called {
		t.Fatal("maintenance LOGIN reached authentication callback")
	}
}
