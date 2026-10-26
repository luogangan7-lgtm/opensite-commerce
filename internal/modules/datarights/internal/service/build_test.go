// Export-worker transaction probes keep cancellation behind committed SQL
// terminal state, including a deadline that expires during construction.
package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type buildTx struct {
	pgx.Tx
	info     string
	build    string
	buildErr error
}

type buildRow struct {
	payload string
	err     error
}

func (tx buildTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "DataRightsTenantContext") {
		return statusRow{tenant: true}
	}
	if strings.Contains(sql, "DataRightsWorkerInfo") {
		return buildRow{payload: tx.info}
	}
	return buildRow{payload: tx.build, err: tx.buildErr}
}

func (r buildRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*[]byte) = []byte(r.payload)
	return nil
}

func TestBuildReturnsPermanentErrorsOnlyAfterTerminalCommit(t *testing.T) {
	commitFailure := errors.New("commit failed")
	for _, tc := range []struct {
		name      string
		tx        buildTx
		commitErr error
		want      error
		committed bool
	}{
		{"record limit", buildTx{info: `{"status":"pending"}`, build: `{"status":"failed"}`}, nil, contract.ErrInvalid, true},
		{"failed replay", buildTx{info: `{"status":"failed"}`}, nil, contract.ErrInvalid, true},
		{"expired", buildTx{info: `{"status":"expired"}`}, nil, contract.ErrConflict, true},
		{"commit fails", buildTx{info: `{"status":"expired"}`}, commitFailure, commitFailure, false},
		{"expires during build", buildTx{info: `{"status":"pending"}`, buildErr: &pgxadapter.PgError{Code: "55000"}}, nil, contract.ErrUnavailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			committed := false
			s := Service{run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
				if err := fn(ctx, tc.tx); err != nil {
					return err
				}
				if tc.commitErr != nil {
					return tc.commitErr
				}
				committed = true
				return nil
			}}
			if err := s.BuildExport(context.Background(), tenant, request); !errors.Is(err, tc.want) {
				t.Fatalf("build error = %v, want %v", err, tc.want)
			}
			if committed != tc.committed {
				t.Fatalf("committed = %t, want %t", committed, tc.committed)
			}
		})
	}
}
