//go:build !pgx

// Responsibility: preserve public route contract without database support.
// Dependencies: httpapi; caller: main; invariant: public reads return unavailable.
package main

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func storefrontRoutes(context.Context) ([]httpapi.Route, func(), error) {
	return []httpapi.Route{httpapi.StorefrontRoute(nil)}, func() {}, nil
}
