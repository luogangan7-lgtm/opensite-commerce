//go:build pgx

// Responsibility: compose public read routes using the dedicated public DSN.
// Dependencies: publish facade/httpapi; caller: main. Invariant: absent DSN fails
// read requests closed; invalid privileged DSN fails startup; no admin fallback.
package main

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"os"
)

func storefrontRoutes(ctx context.Context) ([]httpapi.Route, func(), error) {
	dsn := os.Getenv("OSC_PUBLIC_DATABASE_URL")
	if dsn == "" {
		return []httpapi.Route{httpapi.StorefrontRoute(nil)}, func() {}, nil
	}
	module, close, err := publish.Open(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	return []httpapi.Route{httpapi.StorefrontRoute(module)}, close, nil
}
