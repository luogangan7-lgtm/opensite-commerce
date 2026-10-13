// Package publish exposes a sealed-release read facade to API composition.
// Dependencies: private store/service and public contract; caller: cmd/api.
// Invariants: only dedicated public connections; product content only from the sealed
// release (no draft/live content fallback); SKU price/availability are live.
package publish

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/internal/service"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/internal/store"
)

type Module struct{ store *store.Store }

func Open(ctx context.Context, dsn string) (*Module, func(), error) {
	s, close, err := store.Open(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	return &Module{store: s}, close, nil
}
func (m *Module) Current(ctx context.Context, host string) (contract.Site, error) {
	host, err := contract.NormalizeHost(host)
	if err != nil {
		return contract.Site{}, err
	}
	if m == nil || m.store == nil {
		return contract.Site{}, contract.ErrUnavailable
	}
	s, r, locale, manifest, pages, live, err := m.store.Current(ctx, host)
	if err != nil {
		return contract.Site{}, err
	}
	site, err := service.Project(s, r, host, locale, manifest, pages)
	if err != nil {
		return contract.Site{}, err
	}
	return service.AttachSKUs(ctx, site, live)
}
