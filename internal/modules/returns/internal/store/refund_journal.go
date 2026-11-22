package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	ledger "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
)

// RefundJournal maps trusted journal failures to Returns errors so a temporary
// server failure preserves the HTTP 503 and same-key recovery contract (issue #190).
type RefundJournal struct {
	Journal contract.RefundJournal
}

func (j RefundJournal) LockReturnCheckout(ctx context.Context, tx pgx.Tx, returnID string) error {
	return MapError(j.Journal.LockReturnCheckout(ctx, tx, returnID))
}

func (j RefundJournal) PostReturnRefund(ctx context.Context, tx pgx.Tx, returnID, key, refundID string, posting ledger.Transaction) (string, error) {
	id, err := j.Journal.PostReturnRefund(ctx, tx, returnID, key, refundID, posting)
	return id, MapError(err)
}
