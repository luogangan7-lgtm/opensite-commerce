// Responsibilities: compose optional configured persistence without inventing
// authentication. Dependencies: pgxadapter and module Config ports; cmd/api calls
// this at startup. Each database uses its own login, and errors omit credentials.
package bootstrap

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
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
		config.CMS.Idempotency.Run = runner.RunAuthorizedTx
		if encoded := os.Getenv("OPENSITE_CMS_PREVIEW_KEY"); encoded != "" && config.CMS.Writer == nil {
			key, err := base64.RawURLEncoding.DecodeString(encoded)
			if err != nil || len(key) < 32 {
				closeRuntime()
				return Config{}, nil, errors.New("CMS preview key is invalid")
			}
			writer, err := publish.NewWriter(publish.WriteConfig{Run: runner.RunAuthorizedTx, ReadRun: runner.RunAuthorizedTxSnapshot,
				PreviewKey: key, PreviewRun: func(ctx context.Context, claims contract.PreviewClaims, fn func(context.Context, pgx.Tx) error) error {
					return runner.RunCapabilitySnapshot(ctx, claims.TenantID, fn)
				}})
			if err != nil {
				closeRuntime()
				return Config{}, nil, errors.New("CMS writer configuration is invalid")
			}
			config.CMS.Writer = writer
		}
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
	if root := os.Getenv("OPENSITE_IDEMPOTENCY_ROOT"); root != "" && config.CMS.Idempotency.Store == nil {
		store, err := idempotency.NewLocalResponseStore(root, contract.MaxWriteBytes)
		if err != nil {
			closeRuntime()
			return Config{}, nil, errors.New("command response storage is unavailable")
		}
		config.CMS.Idempotency.Store = store
	}
	return config, closeRuntime, nil
}
