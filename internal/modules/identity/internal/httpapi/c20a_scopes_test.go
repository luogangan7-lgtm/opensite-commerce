// Responsibilities: exercise fulfillment/refund scope admission through Router.
// Dependencies: C02c credential fixtures; default and tagged CI invoke these tests.
// Invariant: only current owner/admin authority plus explicit key scopes admits.
package httpapi

import (
	"net/http"
	"testing"

	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func TestC20aStaffScopeAdmission(t *testing.T) {
	for _, scope := range []string{"fulfillment:write", "returns:write", "unknown:scope", "*"} {
		for _, role := range []string{"owner", "admin", "catalog_editor", "content_editor", "viewer", "unknown"} {
			for _, bearer := range []bool{false, true} {
				kind := "user"
				id := c02cUser
				if bearer {
					kind, id = "api_key", c02cKeyID
				}
				t.Run(scope+"/"+role+"/"+kind, func(t *testing.T) {
					f := c02cAuthFixture(t)
					m := f.authority.memberships[c02cTenant]
					m.Role = role
					f.authority.memberships[c02cTenant] = m
					k := f.authority.keys[c02cKeyID]
					k.Scopes = []string{scope}
					f.authority.keys[c02cKeyID] = k
					status := http.StatusForbidden
					if (role == "owner" || role == "admin") && (scope == "fulfillment:write" || scope == "returns:write") {
						status = http.StatusOK
					}
					w := f.serve(t, c02cSpec(platform.AdminOrAPIKey, "GET", scope), f.request("GET", c02cTenant, !bearer, bearer))
					c02cAssert(t, f, w, status, kind, id, c02cTenant)
				})
			}
		}
	}
	for _, scope := range []string{"fulfillment:write", "returns:write"} {
		for _, fault := range []string{"narrowed_key", "wildcard_key", "cross_tenant_key"} {
			t.Run(scope+"/"+fault, func(t *testing.T) {
				f := c02cAuthFixture(t)
				k := f.authority.keys[c02cKeyID]
				k.Scopes = []string{scope}
				target, status := c02cTenant, http.StatusForbidden
				switch fault {
				case "narrowed_key":
					k.Scopes = []string{"catalog:read"}
				case "wildcard_key":
					k.Scopes = []string{"*"}
				case "cross_tenant_key":
					target, status = c02cOtherTenant, http.StatusNotFound
					m := f.authority.memberships[c02cTenant]
					m.TenantID = target
					f.authority.memberships[target] = m
				}
				f.authority.keys[c02cKeyID] = k
				w := f.serve(t, c02cSpec(platform.AdminOrAPIKey, "GET", scope), f.request("GET", target, false, true))
				c02cAssert(t, f, w, status, "", "", "")
				if f.authority.touches != 0 {
					t.Fatal("denied scope refreshed key activity")
				}
			})
		}
	}
}
