// Package store connects the offline CLI using its dedicated bootstrap login.
// Dependencies: tenanttx identity adapter and generated db; identity service calls.
// Invariant: validate authority before bootstrap in the same READ COMMITTED
// transaction; no driver/SQL/DSN error escapes, no raw connection reaches modules.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

var errDatabase = errors.New("bootstrap database operation failed")

func Bootstrap(ctx context.Context, dsn, email, tenantName, passwordHash string) (string, error) {
	runner, err := pgxadapter.OpenIdentity(ctx, dsn)
	if err != nil {
		return "", errDatabase
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runner.Close(closeCtx)
	}()
	var tenantID string
	err = runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		queries := db.New(tx)
		valid, err := queries.ValidateBootstrapConnection(ctx)
		if err != nil || !valid.Valid || !valid.Bool {
			return errDatabase
		}
		id, err := queries.BootstrapAdmin(ctx, db.BootstrapAdminParams{Email: email, TenantName: tenantName, PasswordHash: passwordHash})
		if err != nil || !id.Valid {
			return errDatabase
		}
		tenantID = id.String()
		return nil
	})
	if err != nil {
		return "", errDatabase
	}
	return tenantID, nil
}
