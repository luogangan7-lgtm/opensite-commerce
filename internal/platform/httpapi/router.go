// Package httpapi owns HTTP transport and default-deny route registration.
// It depends on net/http and platform/errors; bootstrap and module transports
// call it. Authentication must succeed before a private handler can run.
package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"

	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

type Access string

const (
	Public             Access = "Public"
	Storefront         Access = "Storefront"
	AuthenticatedSelf  Access = "AuthenticatedSelf"
	AdminSession       Access = "AdminSession"
	APIKey             Access = "APIKey"
	AdminOrAPIKey      Access = "AdminOrAPIKey"
	InternalService    Access = "InternalService"
	DefaultMaxBodySize int64  = 1 << 20
)

type RouteSpec struct {
	OperationID string
	Method      string
	Pattern     string
	Access      Access
	Scope       string
	MaxBodySize int64
}

type Route struct {
	Spec    RouteSpec
	Handler http.Handler
}

// Authenticator is the PR-02 seam. It must validate the selected Access class,
// identity, tenant and scope, and return the resulting trusted request context.
type Authenticator interface {
	Authenticate(context.Context, *http.Request, RouteSpec) (context.Context, error)
}

type DenyAuthenticator struct{}

func (DenyAuthenticator) Authenticate(context.Context, *http.Request, RouteSpec) (context.Context, error) {
	return nil, apperrors.New(401, "unauthenticated", "Authentication is required")
}

type Router struct {
	mux        *http.ServeMux
	auth       Authenticator
	handler    http.Handler
	mu         sync.Mutex
	operations map[string]RouteSpec
}

func New(auth Authenticator, logger *slog.Logger) *Router {
	if auth == nil {
		auth = DenyAuthenticator{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &Router{mux: http.NewServeMux(), auth: auth, operations: make(map[string]RouteSpec)}
	r.mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		WriteProblem(w, req, apperrors.New(404, "not_found", "Route not found"))
	})
	r.handler = middleware(logger, r.mux)
	return r
}

// Register is called during static startup; all errors must reach bootstrap.
// ServeMux conflicts are configuration errors, not process-level panics.
func (s *Router) Register(spec RouteSpec, handler http.Handler) (err error) {
	if err = validateRoute(spec); err != nil {
		return err
	}
	if handler == nil {
		return fmt.Errorf("route %q: nil handler", spec.OperationID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.operations[spec.OperationID]; exists {
		return fmt.Errorf("duplicate operation %q", spec.OperationID)
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("route %q: invalid or conflicting pattern: %v", spec.OperationID, p)
		}
	}()
	s.mux.Handle(spec.Method+" "+spec.Pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ctx = context.WithValue(ctx, authorizedTenantKey{}, "")
		ctx = context.WithValue(ctx, authorizedActorKey{}, Actor{})
		if spec.Access == Storefront {
			w.Header().Set("Cache-Control", "no-store")
		}
		if spec.Access != Public {
			var err error
			ctx, err = s.auth.Authenticate(ctx, r, spec)
			if err != nil {
				WriteProblem(w, r, err)
				return
			}
			if ctx == nil {
				WriteProblem(w, r, apperrors.New(401, "unauthenticated", "Authentication is required"))
				return
			}
			// An authenticator may return a context from a prior request. Neither
			// identity nor tenant stamps survive admission to a different route.
			ctx = context.WithValue(ctx, authorizedTenantKey{}, "")
			ctx = context.WithValue(ctx, authorizedActorKey{}, Actor{})
			if strings.Contains(spec.Pattern, "{tenant_id}") {
				tenant := r.PathValue("tenant_id")
				if !tenanttx.ValidTenantID(tenant) {
					WriteProblem(w, r, apperrors.New(400, "invalid_tenant", "Tenant must be a canonical UUID"))
					return
				}
				ctx = context.WithValue(ctx, authorizedTenantKey{}, tenant)
			}
			if resolver, ok := s.auth.(ActorResolver); ok {
				actor, err := resolver.ResolveActor(ctx, r, spec)
				if err != nil {
					WriteProblem(w, r, err)
					return
				}
				if actor != (Actor{}) && !validActor(actor) {
					WriteProblem(w, r, apperrors.New(401, "unauthenticated", "Authorized actor context is required"))
					return
				}
				ctx = context.WithValue(ctx, authorizedActorKey{}, actor)
			}
		}
		ctx = context.WithValue(ctx, requestIDKey{}, RequestID(r.Context()))
		ctx = context.WithValue(ctx, bodyLimitKey{}, spec.MaxBodySize)
		handler.ServeHTTP(w, r.WithContext(ctx))
	}))
	s.operations[spec.OperationID] = spec
	return nil
}

// RouteSpecs exposes a copy of successfully registered routes for contract gates.
// It is derived from actual registration, never a second manually maintained list.
func (s *Router) RouteSpecs() []RouteSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	specs := make([]RouteSpec, 0, len(s.operations))
	for _, spec := range s.operations {
		specs = append(specs, spec)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].OperationID < specs[j].OperationID })
	return specs
}

func validateRoute(s RouteSpec) error {
	fail := func(reason string) error { return fmt.Errorf("route %q: %s", s.OperationID, reason) }
	if strings.TrimSpace(s.OperationID) == "" {
		return fail("operation ID required")
	}
	if s.Method == "" || strings.IndexFunc(s.Method, func(c rune) bool { return c < 'A' || c > 'Z' }) >= 0 {
		return fail("uppercase HTTP method required")
	}
	if !strings.HasPrefix(s.Pattern, "/") || strings.ContainsAny(s.Pattern, " ?#\t\n\r") {
		return fail("path pattern required")
	}
	if s.MaxBodySize < 0 {
		return fail("negative body size")
	}
	switch s.Access {
	case Public, Storefront, AuthenticatedSelf, AdminSession, APIKey, AdminOrAPIKey, InternalService:
	default:
		return fail("unknown access class")
	}
	// The tenant path parameter has exactly one spelling, {tenant_id}: any other
	// wildcard naming a tenant ({tenantId}, {Tenant_ID}, {tenant}, ...) would dodge
	// the scope rule below and PR-02's tenant resolver, so it is rejected.
	tenant := false
	for _, segment := range strings.Split(s.Pattern, "/") {
		if !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}"), "...")
		// ASCII snake_case only: lookalike letters (Cyrillic е, fullwidth ｔ) must not
		// smuggle a tenant parameter past the canonical-name check below.
		if name == "" || strings.IndexFunc(name, func(c rune) bool { return !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') }) >= 0 {
			return fail("path wildcard names must be [a-z0-9_]")
		}
		if !strings.Contains(strings.ToLower(name), "tenant") {
			continue
		}
		if name != "tenant_id" {
			return fail("tenant path parameter must be named {tenant_id}")
		}
		tenant = true
	}
	if s.Access == AuthenticatedSelf && (tenant || s.Scope != "") {
		return fail("AuthenticatedSelf cannot declare tenant or scope")
	}
	if s.Access == Public && s.Scope != "" {
		return fail("public route cannot declare a scope")
	}
	if s.Access == Storefront && (!tenant || s.Scope != "" || !strings.HasPrefix(s.Pattern, "/api/v1/t/{tenant_id}/")) {
		return fail("Storefront requires canonical tenant path and no staff scope")
	}
	if s.Access != Public && s.Access != Storefront && tenant && strings.TrimSpace(s.Scope) == "" {
		return fail("private tenant route requires scope")
	}
	return nil
}

// WrapTransport installs an operational wrapper during static startup only.
// It preserves the Router and its business route registry; callers must invoke
// it before serving requests, outside the built-in recovery transport.
func (s *Router) WrapTransport(wrap func(http.Handler) http.Handler) { s.handler = wrap(s.handler) }

func (s *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

type bodyLimitKey struct{}

func maxBodySize(r *http.Request) int64 {
	if limit, ok := r.Context().Value(bodyLimitKey{}).(int64); ok && limit > 0 {
		return limit
	}
	return DefaultMaxBodySize
}
