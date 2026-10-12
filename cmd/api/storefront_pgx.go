//go:build pgx

// Responsibility: compose public read routes using the dedicated public DSN.
// Dependencies: publish facade/httpapi; caller: main. Invariant: absent DSN fails
// read requests closed; invalid privileged DSN fails startup; no admin fallback.
package main

import (
	"context"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"os"
)

func storefrontRoutes(ctx context.Context) ([]httpapi.Route, func(), error) {
	dsn := os.Getenv("OSC_PUBLIC_DATABASE_URL")
	if dsn == "" {
		return []httpapi.Route{publish.StorefrontRoute(nil)}, func() {}, nil
	}
	module, close, err := publish.Open(ctx, dsn)
	if err != nil {
		return nil, nil, errors.New("public database is unavailable or misconfigured")
	}
	return []httpapi.Route{publish.StorefrontRoute(module)}, close, nil
}
