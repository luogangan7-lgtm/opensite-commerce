// Responsibility: worker composition exposes only stable send/store capabilities.
// Dependencies: private service/store and platform outbox/QueueDB; cmd worker calls
// NewWorker. Invariant: unfenced Delivery-only send fails closed; leases required.
package notifications

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/internal/service"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/internal/store"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/outbox"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type Module struct{ worker service.Worker }

func NewWorker(pool pgxadapter.QueueDB, sender Sender) *Module {
	return &Module{worker: service.Worker{Repo: store.Worker{Pool: pool}, Sender: sender}}
}
func OutboxStore(pool pgxadapter.QueueDB) outbox.Store { return store.Worker{Pool: pool} }
func (m *Module) Process(ctx context.Context, l outbox.Lease) error {
	if m == nil {
		return outbox.ErrInvalidInput
	}
	return m.worker.Process(ctx, l)
}
func (m *Module) SendLease(ctx context.Context, l outbox.Lease) error { return m.Process(ctx, l) }
func (m *Module) Send(context.Context, outbox.Delivery) error         { return outbox.ErrInvalidInput }
