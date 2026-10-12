// Package pgxadapter opens dedicated identity connections without business SQL.
// Dependencies: pgx and generated db.DBTX; callers: private identity stores.
// Invariants: bounded connection startup; role validation and error sanitizing
// remain with callers; this adapter creates no pools and holds no credentials.
package pgxadapter

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
)

type Connection interface {
	db.DBTX
	Begin(context.Context) (pgx.Tx, error)
	Close(context.Context) error
}

func Open(ctx context.Context, dsn string) (Connection, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	config.ConnectTimeout = 10 * time.Second
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	return conn, nil
}
