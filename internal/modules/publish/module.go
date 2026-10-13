// Package publish exposes a sealed-release read facade to API composition.
// Dependencies: private store/service and public contract; caller: cmd/api.
// Invariants: only dedicated public connections; serves precomputed serving bytes; no
// draft or live catalog fallback (legacy releases serve with no purchasable SKUs).
package publish

import (
	"context"
	"encoding/json"
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

// Serve returns the exact public wire body. Releases published through the CMS write path
// have a serving row; legacy releases (published outside it) are projected from the sealed
// snapshot with no purchasable SKUs, which equals their fallback body.
func (m *Module) Serve(ctx context.Context, host string) ([]byte, error) {
	host, err := contract.NormalizeHost(host)
	if err != nil {
		return nil, err
	}
	if m == nil || m.store == nil {
		return nil, contract.ErrUnavailable
	}
	body, legacy, err := m.store.Serving(ctx, host)
	if err != nil || legacy == nil {
		return body, err
	}
	site, err := service.Project(legacy.SiteID, legacy.ReleaseID, host, legacy.Locale, legacy.Manifest, legacy.Pages)
	if err != nil {
		return nil, err
	}
	serving, err := service.BuildServing(ctx, service.ServingInput{TenantID: legacy.TenantID, Site: site})
	if err != nil {
		return nil, contract.ErrUnavailable
	}
	return serving.Body, nil
}

// Current decodes Serve for in-process callers and tests.
func (m *Module) Current(ctx context.Context, host string) (contract.PublicSite, error) {
	body, err := m.Serve(ctx, host)
	if err != nil {
		return contract.PublicSite{}, err
	}
	var site contract.PublicSite
	if err = json.Unmarshal(body, &site); err != nil {
		return contract.PublicSite{}, contract.ErrUnavailable
	}
	return site, nil
}
