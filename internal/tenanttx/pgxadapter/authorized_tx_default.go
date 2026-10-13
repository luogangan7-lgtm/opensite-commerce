//go:build !pgx

// Responsibilities: keep the public transaction seam fail-closed without pgx
// runtime wiring. Dependencies: tenant admission; HTTP command configuration
// compiles in default builds but no transaction callback may execute.
package pgxadapter

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func (*QueryRunner) RunAuthorizedTx(ctx context.Context, tenant string, _ func(context.Context, pgx.Tx) error) error {
	if err := authorizedTenant(ctx, tenant); err != nil {
		return err
	}
	return errors.New("runtime database is unavailable")
}
