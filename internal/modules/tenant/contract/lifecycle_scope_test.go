// Responsibility: pin owner-only merchant-deletion scope without enabling routes.
// Dependencies: Identity's public scope contract; CI go lane calls these tests.
// Invariant: neither admin, keys' unknown scopes nor other staff acquire owner authority.
package contract

import (
	"slices"
	"testing"

	auth "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/contract"
)

func TestMerchantDeletionScopeIsOwnerOnly(t *testing.T) {
	for _, role := range []string{"owner", "admin", "catalog_editor", "content_editor", "viewer", "unknown", "*", ""} {
		if got := slices.Contains(auth.RoleScopes(role), "tenant:delete"); got != (role == "owner") {
			t.Fatalf("MDEL01_SCOPE: role %q tenant:delete=%v", role, got)
		}
	}
}
