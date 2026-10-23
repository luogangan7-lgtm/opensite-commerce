//go:build !pgx

// Responsibility: reject configured coordinator storage in non-pgx builds.
// Dependencies: QueryRunner; caller: bootstrap. No fabricated database capability.
package pgxadapter

import (
	"context"
	"errors"
)

func OpenReturnsRunner(context.Context, string) (*QueryRunner, error) {
	return nil, errors.New("returns database configuration requires the pgx build tag")
}
