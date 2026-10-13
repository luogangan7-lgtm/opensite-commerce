// Responsibilities: compose optional configured persistence without inventing
// authentication. Dependencies: pgxadapter and module Config ports; cmd/api calls
// this at startup. Each database uses its own login, and errors omit credentials.
package bootstrap

import (
	"context"
	"encoding/hex"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/catalog"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights"
	rights "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"os"
	"path/filepath"
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
		config.CatalogCSV.Run = runner.RunAuthorizedTx
		config.CatalogCSV.Service = catalog.NewCSV()
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
	if root := os.Getenv("OPENSITE_RESPONSE_ROOT"); root != "" {
		store, err := idempotency.NewLocalResponseStore(root, 1<<20)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		config.Privacy.Responses, config.CatalogCSV.Responses = store, store
	}
	if url := os.Getenv("OPENSITE_PRIVACY_DATABASE_URL"); url != "" {
		if !filepath.IsAbs(config.Media.Root) || (os.Getenv("OPENSITE_PRIVACY_ROOT") != "" && filepath.Clean(os.Getenv("OPENSITE_PRIVACY_ROOT")) != filepath.Clean(config.Media.Root)) {
			closeRuntime()
			return Config{}, nil, errors.New("privacy root must equal the configured media root")
		}

		runner, err := pgxadapter.OpenQueryRunner(ctx, url)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		runners = append(runners, runner)
		tokenKey, err := hex.DecodeString(os.Getenv("PRIVACY_TOKEN_KEY"))
		if err != nil {
			closeRuntime()
			return Config{}, nil, errors.New("invalid privacy token key")
		}
		config.Privacy.Run = runner.RunAuthorizedTx
		config.Privacy.Service, err = datarights.New(rights.Config{Run: runner.RunAuthorizedTx, Root: config.Media.Root, TokenKey: tokenKey})
		if err != nil {
			closeRuntime()
			return Config{}, nil, errors.New("privacy configuration unavailable")
		}
		// Explicit deployment flag means the separately configured privacy worker is
		// available. Without it exports fail before creating any unserviceable job.
		if os.Getenv("PRIVACY_EXPORT_WORKER_ENABLED") == "true" {
			config.Privacy.Enqueue, err = privacyEnqueue(os.Getenv("JOB_SIGNING_KEY"))
			if err != nil {
				closeRuntime()
				return Config{}, nil, err
			}
		}
		if config.Privacy.Responses == nil && config.Media.Root != "" {
			store, err := idempotency.NewLocalResponseStore(filepath.Join(config.Media.Root, "command-responses"), 1<<20)
			if err != nil {
				closeRuntime()
				return Config{}, nil, err
			}
			config.Privacy.Responses = store
		}
	}
	return config, closeRuntime, nil
}
