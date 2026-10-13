// Package customerhttp adapts customer authentication at the platform boundary.
// Dependencies: public customer port and strict shared HTTP admission; bootstrap
// mounts these routes. Invariants: independent Host cookie, exact Origin and
// tenant-bound signed CSRF; staff identity and client-supplied IDs never grant access.
package customerhttp

import (
	"net/http"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

const SessionCookie = "__Host-osc_customer_session"
const ChallengeCookie = "__Host-osc_customer_challenge"

type Config struct {
	Service contract.Service
	Origin string
	Secret []byte
}
type Handler struct{ cfg Config }
func New(cfg Config) *Handler { return &Handler{cfg:cfg} }

func (h *Handler) Routes() []httpapi.Route {
	const base = "/api/v1/t/{tenant_id}/customer"
	items := []struct{ id, method, path string }{
		{"registerCustomer","POST",base+"/registrations"},
		{"verifyCustomerEmail","POST",base+"/verifications"},
		{"getCustomerSession","GET",base+"/session"},
		{"createCustomerSession","POST",base+"/session"},
		{"deleteCustomerSession","DELETE",base+"/session"},
		{"requestCustomerPasswordReset","POST",base+"/password-resets"},
		{"confirmCustomerPasswordReset","POST",base+"/password-resets/confirm"},
	}
	routes:=make([]httpapi.Route,0,len(items))
	for _,item:=range items {
		routes=append(routes,httpapi.Route{Spec:httpapi.RouteSpec{OperationID:item.id,Method:item.method,Pattern:item.path,Access:httpapi.Public,MaxBodySize:4096},Handler:http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){ httpapi.WriteProblem(w,r,apperrors.New(503,"customer_auth_unavailable","Customer authentication is unavailable")) })})
	}
	return routes
}
