// Responsibilities: expose the stable actor admitted by central private routing.
// Dependencies: context, tenant UUID validation and platform errors; callers:
// authenticated command transports. Only Router may stamp a resolved identity;
// a user identity is a user ID, never a session ID or an untrusted request field.
package httpapi

import (
	"context"
	"net/http"

	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

// Actor is an authentication-neutral identity projection. Constructing this
// value does not authorize it; only a successful Router admission supplies it.
type Actor struct{ Kind, ID string }

// ActorResolver is an optional Authenticator extension. ResolveActor receives
// the context returned by Authenticate, after access/tenant/scope admission. It
// must project the authenticated principal, never headers, cookies or route IDs.
// Legacy authenticators may omit it; actor-requiring commands then fail closed.
// Actor{}, nil means the selected route has no actor projection; it never stamps.
type ActorResolver interface {
	ResolveActor(context.Context, *http.Request, RouteSpec) (Actor, error)
}

type authorizedActorKey struct{}

func validActor(actor Actor) bool {
	return (actor.Kind == "user" || actor.Kind == "api_key" || actor.Kind == "service") && tenanttx.ValidTenantID(actor.ID)
}

// AuthorizedActor returns only a validated, privately stamped stable identity.
func AuthorizedActor(ctx context.Context) (Actor, error) {
	if ctx != nil {
		if actor, ok := ctx.Value(authorizedActorKey{}).(Actor); ok && validActor(actor) {
			return actor, nil
		}
	}
	return Actor{}, apperrors.New(401, "unauthenticated", "Authorized actor context is required")
}
