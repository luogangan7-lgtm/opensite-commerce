//go:build !pgx

// Responsibilities: reject configured queue persistence in no-database builds.
// Dependencies: none; environment bootstrap uses this fail-closed build seam.
package bootstrap

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

func privacyEnqueue(string) (func(context.Context, pgx.Tx, string) error, error) {
	return nil, errors.New("privacy queue requires the pgx build tag")
}
