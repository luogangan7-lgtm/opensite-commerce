// Responsibility: bounded worker polling of controlled active-tenant pages.
// Dependencies: tenanttx validation and Dispatcher; called by cmd/worker startup.
// Invariants: validate a complete page before claims, retain keyset progress, and
// stop on ambiguous delivery errors. No payload or underlying error is logged.
package outbox

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

var (
	ErrTenantDiscovery = errors.New("OUTBOX_TENANT_DISCOVERY_FAILED")
	ErrDispatch        = errors.New("OUTBOX_DISPATCH_FAILED")
)

// TenantSource is a controlled capability, not a tenant list supplied by events.
// Empty after starts a traversal; results must be ascending UUIDs after cursor.
type TenantSource interface {
	ActiveTenants(context.Context, string, int32) ([]string, error)
}

type Runner struct {
	Source                     TenantSource
	Dispatcher                 Dispatcher
	PollInterval, CycleTimeout time.Duration
	TenantBatchSize            int32
	EventsPerTenant            int
}

// Run processes one bounded page per poll. The next poll continues after the
// last processed tenant; an empty page resets traversal to discover new tenants.
// ponytail: serial sends keep lease/stop semantics simple; parallelize only when
// measured throughput requires it, retaining one independent tenant transaction.
func (r Runner) Run(ctx context.Context) error {
	if r.Source == nil || r.Dispatcher.Store == nil || r.Dispatcher.Sender == nil ||
		r.PollInterval <= 0 || r.PollInterval > time.Minute || r.CycleTimeout <= 0 || r.CycleTimeout > time.Minute ||
		r.TenantBatchSize < 1 || r.TenantBatchSize > 1000 || r.EventsPerTenant < 1 || r.EventsPerTenant > 1000 {
		return ErrInvalidInput
	}
	var after string
	for {
		if ctx.Err() != nil {
			return nil
		}
		cycleCtx, cancel := context.WithTimeout(ctx, r.CycleTimeout)
		next, err := r.page(cycleCtx, after)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		after = next
		timer := time.NewTimer(r.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (r Runner) page(ctx context.Context, after string) (string, error) {
	ids, err := r.Source.ActiveTenants(ctx, after, r.TenantBatchSize)
	if err != nil {
		return after, ErrTenantDiscovery
	}
	if len(ids) == 0 {
		return "", nil
	}
	if len(ids) > int(r.TenantBatchSize) {
		return after, ErrInvalidInput
	}
	previous := after
	for _, id := range ids {
		if !tenanttx.ValidTenantID(id) || id != strings.ToLower(id) || id <= previous {
			return after, ErrInvalidInput
		}
		previous = id
	}
	for _, id := range ids {
		if _, err := r.Dispatcher.Dispatch(ctx, id, r.EventsPerTenant); err != nil {
			return after, ErrDispatch
		}
		after = id
	}
	return after, nil
}
