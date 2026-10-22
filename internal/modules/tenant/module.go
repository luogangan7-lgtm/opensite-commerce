// Package tenant exposes API-key administration and authorization persistence.
// Dependencies: private tenant SQL store/HTTP transport and identity contracts.
// Callers: static API bootstrap; invariant: zero-value Routes registers without
// IO and fails closed; production shares the dedicated app_auth DSN only.
package tenant

import (
	"context"

	auth "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant/contract"
	transport "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant/internal/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant/internal/store"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type Config struct{ DatabaseURL string }
type Module struct {
	repository contract.Repository
	store      *store.Repository
}

func Open(ctx context.Context, cfg Config) (*Module, error) {
	r, e := store.Open(ctx, cfg.DatabaseURL)
	if e != nil {
		return nil, e
	}
	return &Module{repository: r, store: r}, nil
}
func (m Module) Routes() []platform.Route { return transport.New(m.repository).Routes() }
func (m *Module) Authorization() auth.AuthorizationRepository {
	if m == nil || m.store == nil {
		return nil
	}
	return m.store
}
func (m *Module) Close(ctx context.Context) error {
	if m == nil || m.store == nil {
		return nil
	}
	return m.store.Close(ctx)
}
