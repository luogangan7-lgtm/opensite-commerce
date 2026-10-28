//go:build pgx

// Responsibility: reject invalid claim durations before any transaction query.
// Depends on pgx.Tx facade; ClaimTx is called by authorized transaction owners.
package idempotency

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
)

type ttlTx struct {
	pgx.Tx
	queries int
}

func (t *ttlTx) QueryRow(context.Context, string, ...any) pgx.Row { t.queries++; return ttlRow{} }

type ttlRow struct{}

func (ttlRow) Scan(...any) error { return errors.New("TTL_UNEXPECTED_DB_QUERY") }
func TestClaimTTLRejectsOutsideWindowBeforeQuery(t *testing.T) {
	scope := Scope{TenantID: "11111111-1111-4111-8111-111111111111", ActorKind: "service", ActorID: "22222222-2222-4222-8222-222222222222", Operation: "testTTL", Key: "11111111-1111-4111-8111-111111111111"}
	for _, ttl := range []time.Duration{-time.Second, 0, time.Microsecond - 1, 24*time.Hour + time.Nanosecond, 25 * time.Hour, time.Duration(1<<63 - 1)} {
		t.Run(ttl.String(), func(t *testing.T) {
			tx := &ttlTx{}
			_, err := ClaimTx(context.Background(), tx, scope, [32]byte{}, ttl)
			if !errors.Is(err, ErrInvalidTTL) || tx.queries != 0 {
				t.Fatalf("TTL_WINDOW_REJECTION ttl=%s err=%v queries=%d", ttl, err, tx.queries)
			}
		})
	}
}
