//go:build pgx

// Producer joins the caller's domain transaction using native River InsertTx.
// It reads the already-authorized tenant GUC; it never accepts a tenant from JSON,
// commits a transaction, or uses a pool fallback. Any enqueue error rolls back.
package jobs

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

type Enqueuer struct {
	signer *Signer
	client *river.Client[pgx.Tx]
}

func NewEnqueuer(s *Signer) (*Enqueuer, error) {
	if s == nil || len(s.key) < 32 {
		return nil, errors.New("invalid job signer")
	}
	c, err := river.NewClient(riverpgxv5.New(nil), &river.Config{Schema: "queue"})
	if err != nil {
		return nil, err
	}
	return &Enqueuer{signer: s, client: c}, nil
}

func (e *Enqueuer) InventoryExpireTx(ctx context.Context, tx pgx.Tx, batch int) (result *rivertype.JobInsertResult, err error) {
	if tx == nil {
		return nil, errors.New("job transaction required")
	}
	defer func() {
		if err != nil {
			// River may leave a PostgreSQL transaction aborted. Roll back explicitly,
			// using an independent context even if the caller's context was canceled.
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
			defer cancel()
			if rollbackErr := tx.Rollback(rollbackCtx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
				err = errors.Join(err, rollbackErr)
			}
		}
	}()
	if e == nil || e.signer == nil || e.client == nil {
		return nil, errors.New("job enqueuer required")
	}
	tenant, err := db.New(tx).CurrentJobTenant(ctx)
	if err != nil {
		return nil, err
	}
	args, err := e.signer.inventoryArgs(tenant, batch)
	if err != nil {
		return nil, err
	}
	return e.client.InsertTx(ctx, tx, args, nil)
}
