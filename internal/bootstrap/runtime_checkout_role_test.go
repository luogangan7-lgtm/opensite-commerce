// Responsibility: pin the runtime guest-cart snapshot role boundary.
// Dependencies: checkout actor and generated cart SQL; caller: bootstrap tests.
// Invariant: a login-role snapshot must switch to app_runtime before cart RLS reads.
package bootstrap

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
)

type guestCartSnapshotRow struct{ values []any }

func (r guestCartSnapshotRow) Scan(dest ...any) error {
	if len(dest) != len(r.values) {
		return errors.New("unexpected cart row shape")
	}
	for i, value := range r.values {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(value))
	}
	return nil
}

type guestCartSnapshotRows struct{ pgx.Rows }

func (*guestCartSnapshotRows) Next() bool { return false }
func (*guestCartSnapshotRows) Err() error { return nil }
func (*guestCartSnapshotRows) Close()     {}

type guestCartSnapshotTx struct {
	pgx.Tx
	tenant, cartID, role string
	hash                 []byte
	failRole             bool
	roleSet, cartRead    bool
}

func (tx *guestCartSnapshotTx) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(query, "name: SetCartRuntimeRole"):
		if tx.failRole {
			return pgconn.CommandTag{}, errors.New("role switch refused")
		}
		tx.role = "app_runtime"
		tx.roleSet = true
	case !strings.Contains(query, "name: SetCartContext"):
		return pgconn.CommandTag{}, errors.New("unexpected snapshot write")
	}
	return pgconn.NewCommandTag("SELECT 1"), nil
}

func (tx *guestCartSnapshotTx) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(query, "name: GetCartContext"):
		return guestCartSnapshotRow{[]any{tx.tenant, "", "", tx.role}}
	case strings.Contains(query, "name: GetCart"):
		tx.cartRead = true
		return guestCartSnapshotRow{[]any{
			pgtype.UUID{Bytes: uuid.MustParse(tx.tenant), Valid: true},
			pgtype.UUID{Bytes: uuid.MustParse(tx.cartID), Valid: true},
			pgtype.UUID{}, tx.hash, "USD", int64(1), pgtype.Timestamptz{},
		}}
	default:
		panic("unexpected guest cart snapshot query")
	}
}

func (*guestCartSnapshotTx) Query(_ context.Context, query string, _ ...any) (pgx.Rows, error) {
	if !strings.Contains(query, "name: ListCartLines") {
		return nil, errors.New("unexpected snapshot query")
	}
	return &guestCartSnapshotRows{}, nil
}

func TestGuestCartSnapshotSwitchesToRuntimeRoleBeforeRead(t *testing.T) {
	const tenant = "c0900001-0000-4000-8000-000000000001"
	const cartID = "c2400001-0000-4000-8000-000000000001"
	hash := make([]byte, 32)
	actor, err := checkoutcontract.NewAuthorizedActor(tenant, "guest", hex.EncodeToString(hash))
	if err != nil {
		t.Fatal(err)
	}
	for _, failRole := range []bool{false, true} {
		tx := &guestCartSnapshotTx{tenant: tenant, cartID: cartID, role: "opensite_runtime_login", hash: hash, failRole: failRole}
		err := verifyGuestCartSnapshot(context.Background(), tx, actor, cartID)
		if failRole {
			if err == nil || tx.cartRead {
				t.Fatalf("failed role switch reached cart: err=%v read=%v", err, tx.cartRead)
			}
		} else if err != nil || !tx.roleSet || !tx.cartRead {
			t.Fatalf("valid runtime snapshot rejected: err=%v role=%v read=%v", err, tx.roleSet, tx.cartRead)
		}
	}
}
