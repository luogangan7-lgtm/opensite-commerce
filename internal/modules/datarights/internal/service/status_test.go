// Status probes exercise the SQL metadata boundary with explicit database time
// decisions. Hashes stay internal; the returned DTO contains only capabilities.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type statusTx struct {
	pgx.Tx
	payload []byte
}

type statusRow struct {
	payload []byte
	tenant bool
}

func (tx statusTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	return statusRow{payload: tx.payload, tenant: strings.Contains(sql, "DataRightsTenantContext")}
}

func (r statusRow) Scan(dest ...any) error {
	if r.tenant {
		return dest[0].(*pgtype.UUID).Scan(tenant)
	}
	*dest[0].(*[]byte) = append([]byte(nil), r.payload...)
	return nil
}

func TestStatusCapabilityMatchesStoredHashAndDatabaseClock(t *testing.T) {
	old := Service{key: []byte("01234567890123456789012345678901")}
	rotated := Service{key: []byte("11234567890123456789012345678901")}
	for _, tc := range []struct {
		name      string
		service   Service
		available bool
		deadline  time.Time
		wantToken bool
	}{
		{"current key", old, true, time.Now().Add(time.Hour), true},
		{"rotated key", rotated, true, time.Now().Add(time.Hour), false},
		{"database expired", old, false, time.Now().Add(time.Hour), false},
		{"database valid despite application clock", old, true, time.Unix(1, 0), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hash := sha256.Sum256([]byte(old.token(tenant, request, tc.deadline)))
			payload, err := json.Marshal(map[string]any{
				"id": request, "customer_id": tenant, "status": "ready",
				"download_expires_at": tc.deadline, "download_available": tc.available,
				"token_hash": hex.EncodeToString(hash[:]),
			})
			if err != nil {
				t.Fatal(err)
			}
			out, err := tc.service.Status(context.Background(), statusTx{payload: payload}, tenant, request, tenant)
			if err != nil {
				t.Fatal(err)
			}
			if (out.DownloadToken != "") != tc.wantToken {
				t.Fatalf("advertised capability = %t, want %t", out.DownloadToken != "", tc.wantToken)
			}
			wire, err := json.Marshal(out)
			if err != nil || strings.Contains(string(wire), "token_hash") || strings.Contains(string(wire), "download_available") {
				t.Fatalf("internal metadata reached public response: %s (%v)", wire, err)
			}
		})
	}
}
