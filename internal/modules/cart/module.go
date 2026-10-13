// Package cart is the storefront cart composition boundary. It depends on its
// public contract, private service/store and platform HTTP authentication; bootstrap
// calls it. Only a trusted customer resolver or a guest capability grants access.
package cart

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/internal/service"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/internal/store"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

// ActorResolver is a trusted customer-session seam, never a client identity DTO.
// A zero actor means guest. Customer login/cookies are supplied by a later module.
type ActorResolver func(context.Context, *http.Request) (contract.AuthorizedActor, error)

type Module struct {
	*service.Service
	resolver ActorResolver
	limiter  *createLimiter
	// unconfigured: bootstrap fallback without storage must answer
	// cart_unavailable, never spend or exhaust the anonymous quota.
	unconfigured bool
}

func New(repo contract.Repository, resolver ActorResolver) *Module {
	return newModule(repo, resolver, AnonymousCreateLimit{})
}
func newModule(repo contract.Repository, resolver ActorResolver, limits AnonymousCreateLimit) *Module {
	return &Module{Service: service.New(repo), resolver: resolver, limiter: newCreateLimiter(limits), unconfigured: repo == nil}
}

// Config receives the platform's authorized tenant runners (bootstrap wires
// pgxadapter.QueryRunner.RunAuthorized / RunAuthorizedSnapshot); never a pool.
type Config struct {
	Run, Read            func(context.Context, string, func(*db.Queries) error) error
	AnonymousCreateLimit AnonymousCreateLimit
}

func NewPostgres(config Config, resolver ActorResolver) *Module {
	return newModule(store.New(config.Run, config.Read), resolver, config.AnonymousCreateLimit)
}

// WithActorResolver replaces only the composition seam on a shallow copy.
// Bootstrap installs verified customer context without changing cart business logic.
func (m *Module) WithActorResolver(resolver ActorResolver) *Module {
	copy := *m
	copy.resolver = resolver
	return &copy
}

func (m *Module) Authenticate(ctx context.Context, r *http.Request, spec platform.RouteSpec) (context.Context, error) {
	denied := func() (context.Context, error) { return ctx, apperrors.New(404, "cart_not_found", "Cart not found") }
	tenant := strings.ToLower(r.PathValue("tenant_id"))
	if spec.Access != platform.Storefront || !service.ValidID(tenant) {
		return denied()
	}
	var actor contract.AuthorizedActor
	if m.resolver != nil {
		var err error
		actor, err = m.resolver(ctx, r)
		if err != nil {
			return ctx, err
		}
	}
	tokens := r.Header.Values("X-Cart-Token")
	if actor.CustomerID != "" {
		if !service.ValidID(actor.CustomerID) || strings.ToLower(actor.TenantID) != tenant || len(actor.GuestTokenHash) != 0 || len(tokens) != 0 {
			return denied()
		}
		actor.TenantID = tenant
		actor.CustomerID = strings.ToLower(actor.CustomerID)
		return contract.WithActor(ctx, actor), nil
	}
	// The guest branch never accepts a partially populated trusted identity.
	if actor.TenantID != "" || len(actor.GuestTokenHash) != 0 {
		return denied()
	}
	actor.TenantID = tenant
	if spec.OperationID == "createCart" && len(tokens) == 0 {
		if m.unconfigured {
			return contract.WithActor(ctx, actor), nil
		}
		if !m.limiter.allow(r.RemoteAddr, nil) {
			return ctx, createLimited()
		}
		return contract.WithActor(ctx, actor), nil
	}
	if len(tokens) != 1 {
		return denied()
	}
	raw, err := base64.RawURLEncoding.DecodeString(tokens[0])
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != tokens[0] {
		return denied()
	}
	hash := sha256.Sum256([]byte(tokens[0]))
	actor.GuestTokenHash = hash[:]
	if spec.OperationID == "createCart" && !m.unconfigured && !m.limiter.allow(r.RemoteAddr, actor.GuestTokenHash) {
		return ctx, createLimited()
	}
	return contract.WithActor(ctx, actor), nil
}
