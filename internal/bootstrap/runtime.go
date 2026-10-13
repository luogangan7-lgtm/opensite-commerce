// Responsibilities: compose optional configured persistence without inventing
// authentication. Dependencies: pgxadapter and module Config ports; cmd/api calls
// this at startup. Each database uses its own login, and errors omit credentials.
package bootstrap

import (
	"context"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"os"
)

func FromEnvironment(ctx context.Context, config Config) (Config, func(), error) {
	var runners []*pgxadapter.QueryRunner
	closeRuntime := func() {
		for _, runner := range runners {
			runner.Close()
		}
	}
	if url := os.Getenv("OPENSITE_DATABASE_URL"); url != "" {
		runner, err := pgxadapter.OpenQueryRunner(ctx, url)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		runners = append(runners, runner)
		config.Catalog.Run = runner.RunAuthorized
		config.Fulfillment.Run = runner.RunAuthorizedTx
		config.Returns.Run = runner.RunAuthorizedTx
		if config.Cart == nil {
			config.Cart = cart.NewPostgres(cart.Config{Run: runner.RunAuthorized, Read: runner.RunAuthorizedSnapshot}, nil)
		}
	}
	if url := os.Getenv("OPENSITE_MEDIA_DATABASE_URL"); url != "" {
		runner, err := pgxadapter.OpenQueryRunner(ctx, url)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		runners = append(runners, runner)
		config.Media.Run = runner.RunAuthorized
	}
	if config.Media.Root == "" {
		config.Media.Root = os.Getenv("OPENSITE_MEDIA_ROOT")
	}
	if root := os.Getenv("OPENSITE_IDEMPOTENCY_ROOT"); root != "" && (config.Fulfillment.Idempotency.Store == nil || config.Returns.Idempotency.Store == nil) {
		store, err := idempotency.NewLocalResponseStore(root, 1<<20)
		if err != nil {
			closeRuntime()
			return Config{}, nil, errors.New("command response persistence could not be configured")
		}
		if config.Fulfillment.Idempotency.Store == nil {
			config.Fulfillment.Idempotency.Store = store
		}
		if config.Returns.Idempotency.Store == nil {
			config.Returns.Idempotency.Store = store
		}
	}
	return config, closeRuntime, nil
}
