// Package store connects the offline CLI using its dedicated bootstrap login.
// Dependencies: identitydb adapter and generated db; called by identity service.
// Invariant: validate authority before bootstrap; no driver/SQL/DSN error escapes.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/identitydb/pgxadapter"
)

var errDatabase = errors.New("bootstrap database operation failed")

func Bootstrap(ctx context.Context, dsn, email, tenantName, passwordHash string) (string, error) {
	conn, err := pgxadapter.Open(ctx, dsn)
	if err != nil {
		return "", errDatabase
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	queries := db.New(conn)
	valid, err := queries.ValidateBootstrapConnection(ctx)
	if err != nil || !valid.Valid || !valid.Bool {
		return "", errDatabase
	}
	id, err := queries.BootstrapAdmin(ctx, db.BootstrapAdminParams{Email: email, TenantName: tenantName, PasswordHash: passwordHash})
	if err != nil || !id.Valid {
		return "", errDatabase
	}
	return id.String(), nil
}
