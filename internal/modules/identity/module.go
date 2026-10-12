// Package identity exposes the offline owner-bootstrap port to the CLI.
// Dependencies: private service/store; owns users/authentication writes through
// the database credential-writer function. Invariant: no HTTP bootstrap endpoint.
package identity

import (
	"context"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/service"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/store"
)

// BootstrapRequest carries input exclusively for the controlled offline CLI.
// Password and DatabaseURL must never be logged or placed in command flags.
type BootstrapRequest = service.Request

func BootstrapAdmin(ctx context.Context, input BootstrapRequest) (string, error) {
	return service.Bootstrap(ctx, input, store.Bootstrap)
}
