// Package httpapi owns HTTP transport and default-deny route registration.
// It depends on net/http and platform/errors; bootstrap and module transports
// call it. Authentication must succeed before a private handler can run.
package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
)

type Access string

const (
	Public             Access = "Public"
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
	operations map[string]bool
}

func New(auth Authenticator, logger *slog.Logger) *Router {
	if auth == nil {
		auth = DenyAuthenticator{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &Router{mux: http.NewServeMux(), auth: auth, operations: make(map[string]bool)}
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
	if s.operations[spec.OperationID] {
		return fmt.Errorf("duplicate operation %q", spec.OperationID)
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("route %q: invalid or conflicting pattern: %v", spec.OperationID, p)
		}
	}()
	s.mux.Handle(spec.Method+" "+spec.Pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
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
		}
		ctx = context.WithValue(ctx, requestIDKey{}, RequestID(r.Context()))
		ctx = context.WithValue(ctx, bodyLimitKey{}, spec.MaxBodySize)
		handler.ServeHTTP(w, r.WithContext(ctx))
	}))
	s.operations[spec.OperationID] = true
	return nil
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
	case Public, AuthenticatedSelf, AdminSession, APIKey, AdminOrAPIKey, InternalService:
	default:
		return fail("unknown access class")
	}
	tenant := false
	for _, segment := range strings.Split(s.Pattern, "/") {
		if segment == "{tenant_id}" || segment == "{tenant_id...}" || segment == "{tenant}" || segment == "{tenant...}" {
			tenant = true
		}
	}
	if s.Access == AuthenticatedSelf && (tenant || s.Scope != "") {
		return fail("AuthenticatedSelf cannot declare tenant or scope")
	}
	if s.Access == Public && s.Scope != "" {
		return fail("public route cannot declare a scope")
	}
	if s.Access != Public && tenant && strings.TrimSpace(s.Scope) == "" {
		return fail("private tenant route requires scope")
	}
	return nil
}

func (s *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

type bodyLimitKey struct{}

func maxBodySize(r *http.Request) int64 {
	if limit, ok := r.Context().Value(bodyLimitKey{}).(int64); ok && limit > 0 {
		return limit
	}
	return DefaultMaxBodySize
}
