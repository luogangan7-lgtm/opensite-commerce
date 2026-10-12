// Responsibilities: expose the tenant authorized by central private routing.
// Dependencies: context/platform errors; callers: tenant-aware modules/adapters.
// Invariant: the unexported stamp can only be produced after authentication.
package httpapi

import (
	"context"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
)

type authorizedTenantKey struct{}

func AuthorizedTenant(ctx context.Context) (string, error) {
	if ctx != nil {
		if tenant, ok := ctx.Value(authorizedTenantKey{}).(string); ok && tenant != "" {
			return tenant, nil
		}
	}
	return "", apperrors.New(401, "unauthenticated", "Authorized tenant context is required")
}
