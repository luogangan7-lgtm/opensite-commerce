// Package ledger statically exposes the ledger command/read port to bootstrap.
// Dependencies: pgx and private ledger service; values live in public contract.
// Invariants: caller supplies an authorized tenant transaction and owns its
// lifecycle; this module creates no HTTP handler, connection pool, or auth path.
package ledger

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/internal/service"
	"time"
)

type Module struct{}

func New() *Module { return &Module{} }
func (*Module) Post(ctx context.Context, tx pgx.Tx, input contract.Transaction) (string, error) {
	return service.Post(ctx, tx, input)
}
func (*Module) Balance(ctx context.Context, tx pgx.Tx, tenantID, accountID string, cutoff time.Time) (contract.Balance, error) {
	return service.Balance(ctx, tx, tenantID, accountID, cutoff)
}
