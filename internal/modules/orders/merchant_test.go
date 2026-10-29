// Responsibility: pin fail-closed public merchant owner-port admission.
// Dependencies: Orders public port; SQL behavior is proved separately on PG18.
// Invariant: invalid contexts cannot be reported as known-zero obligations.
package orders

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
)

func TestMerchantDeletionRejectsUntrustedContext(t *testing.T) {
	port := MerchantDeletion{}
	ctx := context.Background()
	if _, err := port.DeletionObligations(ctx, nil, "019a0000-0000-7000-8000-000000000001"); err == nil {
		t.Fatal("missing transaction admitted")
	}
	if _, err := port.DeletionObligations(ctx, &db.Queries{}, "invalid"); err == nil {
		t.Fatal("invalid tenant admitted")
	}
	if _, err := port.EraseMerchant(ctx, &db.Queries{}, "invalid", "invalid", 0); err == nil {
		t.Fatal("invalid erasure admitted")
	}
}

// Embed only the unused DBTX methods; unexpected query shapes panic in this
// adapter-boundary test. Real SQL/tenant/quantity witnesses remain in PG gates.
type merchantWitnessDB struct {
	db.DBTX
	body string
}

type merchantWitnessRow string

func (r merchantWitnessRow) Scan(dest ...any) error {
	*dest[0].(*[]byte) = []byte(r)
	return nil
}

func (x merchantWitnessDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return merchantWitnessRow(x.body)
}

func TestMerchantObligationRejectsMissingAuthority(t *testing.T) {
	ctx := context.Background()
	port := MerchantDeletion{}
	tenantID := "019a0000-0000-7000-8000-000000000001"
	for _, body := range []string{"null", "{}", `{"count":0}`, `{"object_ids":[]}`, `{"count":-1,"object_ids":[]}`, `{"count":1,"object_ids":["foreign"]}`} {
		if _, err := port.DeletionObligations(ctx, db.New(merchantWitnessDB{body: body}), tenantID); err == nil {
			t.Fatalf("missing or invalid authority admitted: %s", body)
		}
	}
	items, err := port.DeletionObligations(ctx, db.New(merchantWitnessDB{body: `{"count":0,"object_ids":[]}`}), tenantID)
	if err != nil || len(items) != 1 || !items[0].Known || items[0].Count != 0 {
		t.Fatalf("explicit zero rejected: %+v %v", items, err)
	}
}
