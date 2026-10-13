//go:build pgx

// Responsibilities: compose the native caller-Tx River producer when configured.
// Dependencies: signed jobs; environment bootstrap alone creates this capability.
package bootstrap

import (
	"context"
	"encoding/hex"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/jobs"
)

func privacyEnqueue(encoded string) (func(context.Context, pgx.Tx, string) error, error) {
	key, err := hex.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("invalid job signing key")
	}
	signer, err := jobs.NewSigner(key)
	if err != nil {
		return nil, err
	}
	producer, err := jobs.NewEnqueuer(signer)
	if err != nil {
		return nil, err
	}
	return producer.CustomerExportTx, nil
}
