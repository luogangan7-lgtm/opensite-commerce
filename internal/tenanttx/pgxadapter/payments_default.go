//go:build !pgx

// Responsibility: keep payment persistence fail-closed without the pgx build.
// Dependencies: public callback types; bootstrap uses the same API in both builds.
package pgxadapter

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

func (*QueryRunner) RunFakePaymentTx(context.Context, string, func(context.Context, pgx.Tx, string, time.Time) error) error {
	return errors.New("payment database requires pgx build")
}

func (*QueryRunner) RunStripePaymentTx(context.Context, string, func(context.Context, pgx.Tx, string, time.Time) error) error {
	return errors.New("payment database requires pgx build")
}
