//go:build pgx

// Responsibilities: adapt C-04 claims to the transport command port.
// Dependencies: idempotency pgx primitives; caller owns tenant transactions.
// Invariant: opaque claim token never leaves the completion closure.
package httpapi

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"time"
)

func claimCommandTx(ctx context.Context, tx pgx.Tx, scope CommandScope, hash [32]byte, ttl time.Duration, store idempotency.ResponseStore) (CommandCompletion, *idempotency.Response, error) {
	result, err := idempotency.ClaimTx(ctx, tx, idempotency.Scope{TenantID: scope.TenantID, ActorKind: scope.ActorKind, ActorID: scope.ActorID, Operation: scope.Operation, Key: scope.Key}, hash, ttl)
	if err != nil {
		return nil, nil, err
	}
	if result.Claim == nil {
		response, err := idempotency.Replay(ctx, store, result.ReplayReference, result.ReplayStatus)
		return nil, &response, err
	}
	return func(ctx context.Context, tx pgx.Tx, response idempotency.Response) error {
		return idempotency.CompleteTx(ctx, tx, result.Claim, response, store)
	}, nil, nil
}
