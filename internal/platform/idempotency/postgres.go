//go:build pgx

// The DAO calls only generated, parameterized reliability queries. Authorized
// handlers supply a scoped pgx transaction; claim, domain writes and completion
// share it. This package never commits, reuses or publishes the transaction.
// Durable responses precede SQL completion; rollback can leave a harmless blob
// orphan, but cannot leave a committed domain write without its response record.
package idempotency

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

var ErrInvalidScope = errors.New("invalid idempotency scope")
var ErrInvalidTTL = errors.New("idempotency TTL must be at least one microsecond")
var ErrStaleClaim = errors.New("idempotency claim is no longer current")
var ErrInvalidRecord = errors.New("invalid idempotency record")

// Scope MUST be derived from the authorized actor and registered operation,
// never client JSON. The complete existing 007 primary key scopes every claim.
type Scope struct{ TenantID, ActorKind, ActorID, Operation, Key string }

// Claim has private ownership fields: callers cannot construct a fenced owner.
type Claim struct {
	scope Scope
	token pgtype.UUID
	hash  [32]byte
}
type Result struct {
	Claim           *Claim
	ReplayReference string
	ReplayStatus    int
}

// ClaimTx compares fingerprints BEFORE the state/token branches: even a
// completed or expired key used for another request is 422, never a replay.
func ClaimTx(ctx context.Context, tx pgx.Tx, scope Scope, hash [32]byte, ttl time.Duration) (Result, error) {
	if tx == nil || !validScope(scope) {
		return Result{}, ErrInvalidScope
	}
	if ttl.Microseconds() < 1 {
		return Result{}, ErrInvalidTTL
	}
	queries := db.New(tx)
	if err := checkTenant(ctx, queries, scope.TenantID); err != nil {
		return Result{}, err
	}
	token, err := newToken()
	if err != nil {
		return Result{}, err
	}
	record, err := queries.ClaimIdempotency(ctx, db.ClaimIdempotencyParams{
		TenantID: parseUUID(scope.TenantID), ActorKind: scope.ActorKind, ActorID: parseUUID(scope.ActorID), Operation: scope.Operation, IdempotencyKey: scope.Key, RequestHash: hash[:], ClaimTtl: pgtype.Interval{Microseconds: ttl.Microseconds(), Valid: true}, ClaimToken: token,
	})
	if err != nil {
		return Result{}, err
	}
	if !bytes.Equal(record.RequestHash, hash[:]) {
		return Result{}, apperrors.New(422, "idempotency_key_reused", "The key was already used for another request")
	}
	switch record.State {
	case "completed":
		if record.ResponseStatus < 200 || record.ResponseStatus > 599 || record.ResponseBodyReference == "" || len(record.ResponseBodyReference) > 2048 {
			return Result{}, ErrInvalidRecord
		}
		return Result{ReplayReference: record.ResponseBodyReference, ReplayStatus: int(record.ResponseStatus)}, nil
	case "in_progress":
		if record.ClaimToken == token {
			return Result{Claim: &Claim{scope, token, hash}}, nil
		}
		return Result{}, apperrors.New(409, "idempotency_in_progress", "The original request is still processing")
	default:
		return Result{}, ErrInvalidRecord
	}
}

// CompleteTx persists the exact response and fences SQL with SET LOCAL's
// transaction-scoped set_config equivalent AND a current-token WHERE clause.
// It cannot complete an old owner's record after takeover, nor alter a result.
func CompleteTx(ctx context.Context, tx pgx.Tx, claim *Claim, response Response, store ResponseStore) error {
	if tx == nil || claim == nil || store == nil || !validScope(claim.scope) {
		return ErrInvalidScope
	}
	checked, err := validateResponse(response, int64(len(response.Body)))
	if err != nil {
		return err
	}
	queries := db.New(tx)
	if err = checkTenant(ctx, queries, claim.scope.TenantID); err != nil {
		return err
	}
	reference, err := store.Put(ctx, checked)
	if err != nil {
		return err
	}
	if reference == "" || len(reference) > 2048 {
		return ErrInvalidReference
	}
	if _, err = queries.SetIdempotencyClaim(ctx, uuidText(claim.token)); err != nil {
		return err
	}
	count, err := queries.CompleteIdempotency(ctx, db.CompleteIdempotencyParams{
		TenantID: parseUUID(claim.scope.TenantID), ActorKind: claim.scope.ActorKind, ActorID: parseUUID(claim.scope.ActorID), Operation: claim.scope.Operation, IdempotencyKey: claim.scope.Key, RequestHash: claim.hash[:], ClaimToken: claim.token, ResponseStatus: int32(checked.Status), ResponseBodyReference: reference,
	})
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrStaleClaim
	}
	return nil
}

// ExecuteTx keeps domain writes and the completed response in ONE caller-owned
// authorized transaction. The caller commits before writing any HTTP bytes.
// An error/panic/cancellation must cause the caller's tenant runner to roll back.
func ExecuteTx(ctx context.Context, tx pgx.Tx, scope Scope, hash [32]byte, ttl time.Duration, store ResponseStore, command func(context.Context, pgx.Tx) (Response, error)) (Response, error) {
	if store == nil || command == nil {
		return Response{}, ErrInvalidScope
	}
	result, err := ClaimTx(ctx, tx, scope, hash, ttl)
	if err != nil {
		return Response{}, err
	}
	if result.Claim == nil {
		return Replay(ctx, store, result.ReplayReference, result.ReplayStatus)
	}
	response, err := command(ctx, tx)
	if err != nil {
		return Response{}, err
	}
	if err = CompleteTx(ctx, tx, result.Claim, response, store); err != nil {
		return Response{}, err
	}
	return validateResponse(response, int64(len(response.Body)))
}
func checkTenant(ctx context.Context, queries *db.Queries, tenant string) error {
	current, err := queries.ReadIdempotencyTenant(ctx)
	if err != nil {
		return err
	}
	if current != tenant {
		return apperrors.New(403, "tenant_scope_mismatch", "The transaction is not scoped to the authorized tenant")
	}
	return nil
}
func validScope(scope Scope) bool {
	if !tenanttx.ValidTenantID(scope.TenantID) || !tenanttx.ValidTenantID(scope.ActorID) || !validKey(scope.Key) || scope.Operation == "" || len(scope.Operation) > 255 {
		return false
	}
	if scope.ActorKind != "user" && scope.ActorKind != "api_key" && scope.ActorKind != "service" {
		return false
	}
	for i := range len(scope.Operation) {
		if scope.Operation[i] < 0x20 || scope.Operation[i] > 0x7e {
			return false
		}
	}
	return true
}
func parseUUID(id string) pgtype.UUID { var result pgtype.UUID; _ = result.Scan(id); return result }
func newToken() (pgtype.UUID, error) {
	var result pgtype.UUID
	if _, err := rand.Read(result.Bytes[:]); err != nil {
		return result, err
	}
	result.Bytes[6] = result.Bytes[6]&15 | 64
	result.Bytes[8] = result.Bytes[8]&63 | 128
	result.Valid = true
	return result, nil
}
func uuidText(id pgtype.UUID) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", id.Bytes[:4], id.Bytes[4:6], id.Bytes[6:8], id.Bytes[8:10], id.Bytes[10:])
}
