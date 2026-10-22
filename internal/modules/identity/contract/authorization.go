// Package contract defines neutral authorization records and tenant-owned ports.
// Dependencies: context and generated transaction queries; identity consumes
// these ports and tenant implements them. Active records use the database clock;
// no secret or raw database connection crosses this module boundary.
package contract

import (
	"context"
	"errors"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
)

var (
	ErrNotFound    = errors.New("authorization record not found")
	ErrUnavailable = errors.New("authorization is unavailable")
)

type Membership struct{ UserID, TenantID, Role string }
type APIKey struct {
	ID, TenantID, UserID string
	Hash                 [32]byte
	Scopes               []string
}

// AuthorizationRepository returns only active users, tenants and memberships;
// Key also filters expiry/revocation using the database clock. TouchKey repeats
// that validity check before recording successful use.
type AuthorizationRepository interface {
	Membership(context.Context, string, string) (Membership, error)
	Key(context.Context, string) (APIKey, error)
	TouchKey(context.Context, string) error
}

const (
	KindSession = "session"
	KindAPIKey  = "api_key"
)

// TransactionAuthorization contains admitted identity and the exact route scope.
// CredentialID is a session ID or public key ID, never a token or secret.
type TransactionAuthorization struct{ UserID, Kind, CredentialID, Scope string }

// TransactionAuthorizer rechecks current authority and retains its locks in the
// caller's business transaction. It must not borrow the pre-request auth pool.
type TransactionAuthorizer interface {
	Revalidate(context.Context, *db.Queries, string, TransactionAuthorization) error
}

// RoleScopes returns a fresh exact allowlist. Unknown roles and wildcards have
// no authority; SQL capability checks mirror this same fixed mapping.
func RoleScopes(role string) []string {
	switch role {
	case "owner", "admin":
		return []string{"organization.create", "site.create", "catalog:read", "catalog:write", "media:write", "cms:write", "cms:publish", "quotes:read", "quotes:write", "notifications:write", "orders:read", "apikeys:write", "fulfillment:write", "returns:write"}
	case "catalog_editor":
		return []string{"catalog:read", "catalog:write", "media:write"}
	case "content_editor":
		return []string{"catalog:read", "cms:write", "cms:publish", "media:write"}
	case "viewer":
		return []string{"catalog:read"}
	default:
		return nil
	}
}
