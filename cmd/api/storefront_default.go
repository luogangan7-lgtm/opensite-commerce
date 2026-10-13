//go:build !pgx

// Responsibility: preserve public route contract without database support.
// Dependencies: publish/httpapi; caller: main; invariant: public reads return unavailable.
package main

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func storefrontRoutes(context.Context) ([]httpapi.Route, func(), error) {
	return []httpapi.Route{publish.StorefrontRoute(nil)}, func() {}, nil
}
