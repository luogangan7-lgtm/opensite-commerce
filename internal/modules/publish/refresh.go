// Responsibility: periodic storefront serving refresh (ADR_STOREFRONT_SERVING 2.6).
// Dependencies: publish store (builder, due query); caller: bootstrap composition.
// Invariants: tenants come from trusted deployment configuration only; every build runs
// in a tenant transaction under the publication row lock and CAS-writes the row; when the
// refresher is absent or down, invalidated rows fall back within the 30 s grace and every
// row within the max lifetime.
package publish

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/internal/store"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

// RefreshConfig composes the refresher. Run must be a tenant write transaction for a
// trusted internal caller (no HTTP actor); Tenants is the deployment-authorized list.
type RefreshConfig struct {
	Run      store.Transaction
	Tenants  []string
	Interval time.Duration
	Logger   *slog.Logger
}

type Refresher struct{ c RefreshConfig }

func NewRefresher(c RefreshConfig) (*Refresher, error) {
	if c.Run == nil || len(c.Tenants) == 0 {
		return nil, errors.New("serving refresher requires a transaction and tenants")
	}
	seen := map[string]bool{}
	for _, t := range c.Tenants {
		if !tenanttx.ValidTenantID(t) || seen[t] {
			return nil, errors.New("invalid serving refresh tenant list")
		}
		seen[t] = true
	}
	if c.Interval == 0 {
		c.Interval = 10 * time.Second
	}
	if c.Interval < time.Second {
		return nil, errors.New("serving refresh interval must be at least one second")
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return &Refresher{c: c}, nil
}

// Sweep refreshes every due serving row of every configured tenant once.
func (r *Refresher) Sweep(ctx context.Context) (refreshed int, err error) {
	for _, tenant := range r.c.Tenants {
		n, e := store.RefreshDue(ctx, r.c.Run, tenant)
		refreshed += n
		if e != nil && err == nil {
			err = e
		}
	}
	return refreshed, err
}

// Run sweeps immediately and then every Interval until ctx is done.
func (r *Refresher) Run(ctx context.Context) {
	ticker := time.NewTicker(r.c.Interval)
	defer ticker.Stop()
	for {
		if _, err := r.Sweep(ctx); err != nil && ctx.Err() == nil {
			r.c.Logger.Warn("storefront serving refresh incomplete", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
