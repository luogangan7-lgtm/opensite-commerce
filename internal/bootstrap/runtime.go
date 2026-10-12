// Responsibilities: compose optional configured persistence without inventing
// authentication. Dependencies: pgxadapter and module Config ports; cmd/api calls
// this at startup. Each database uses its own login, and errors omit credentials.
package bootstrap

import (
	"context"
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
	return config, closeRuntime, nil
}
