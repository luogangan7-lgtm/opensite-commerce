// Responsibility: test private persistence transaction and error boundaries.
// Dependencies: generated db queries with a transaction callback runner double.
// Callers: focused Go gate; invariants: lost rotation writes no successor,
// failed device insert rolls back, commit failure leaks no driver detail.
package store

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
)

type loginFakeRow struct {
	err  error
	user pgtype.UUID
	at   time.Time
}

func (r loginFakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	id, _ := uuid("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	*dest[0].(*pgtype.UUID) = id
	*dest[1].(*pgtype.UUID) = r.user
	*dest[2].(*pgtype.Timestamptz) = stamp(r.at)
	*dest[3].(*pgtype.Timestamptz) = stamp(r.at)
	*dest[4].(*pgtype.Timestamptz) = stamp(r.at.Add(time.Hour))
	return nil
}

type loginFakeTx struct {
	pgx.Tx
	writes                   []string
	rotated                  bool
	rejectDevice, failCommit bool
	noRow                    bool
	user                     pgtype.UUID
	at                       time.Time
	committed, rolledBack    bool
	commitCalls              int
}

func (tx *loginFakeTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "LoginRotateOldSession") {
		tx.writes = append(tx.writes, "rotate")
		if tx.rotated {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		tx.rotated = true
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	tx.writes = append(tx.writes, "device")
	if tx.rejectDevice {
		return pgconn.CommandTag{}, errors.New("secret DSN backend details")
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}
func (tx *loginFakeTx) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	tx.writes = append(tx.writes, "session")
	if tx.noRow {
		return loginFakeRow{err: pgx.ErrNoRows}
	}
	return loginFakeRow{user: tx.user, at: tx.at}
}
func (tx *loginFakeTx) Commit(context.Context) error {
	tx.commitCalls++
	if tx.failCommit {
		return errors.New("private commit details")
	}
	tx.committed = true
	return nil
}
func (tx *loginFakeTx) Rollback(context.Context) error { tx.rolledBack = true; return nil }

type loginFakeRunner struct {
	tx       pgx.Tx
	calls    int
	closeErr error
}

func (r *loginFakeRunner) InMaintenance(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	r.calls++
	if err := fn(ctx, r.tx); err != nil {
		_ = r.tx.Rollback(ctx)
		return err
	}
	if err := r.tx.Commit(ctx); err != nil {
		_ = r.tx.Rollback(ctx)
		return err
	}
	return nil
}
func (r *loginFakeRunner) Close(context.Context) error { return r.closeErr }
func loginFixture() (*LoginRepository, *loginFakeTx, login.SessionInput) {
	at := time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
	u, _ := uuid("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	tx := &loginFakeTx{user: u, at: at}
	runner := &loginFakeRunner{tx: tx}
	r := &LoginRepository{runner: runner, gate: make(chan struct{}, 1), idle: 30 * time.Minute}
	old := [32]byte{1}
	in := login.SessionInput{UserID: u.String(), TokenHash: [32]byte{2}, DeviceHash: [32]byte{3}, CreatedAt: at, ExpiresAt: at.Add(time.Hour), DeviceExpiresAt: at.Add(24 * time.Hour), OldTokenHash: &old}
	return r, tx, in
}
func TestLostRotationNeverInsertsSuccessor(t *testing.T) {
	r, tx, in := loginFixture()
	tx.rotated = true
	_, err := r.SaveLogin(context.Background(), in)
	if !errors.Is(err, login.ErrSession) || strings.Join(tx.writes, ",") != "rotate" || tx.commitCalls != 0 || tx.committed || !tx.rolledBack || r.runner.(*loginFakeRunner).calls != 1 {
		t.Fatalf("lost rotation must rollback before successor: err=%v writes=%v commit=%v rollback=%v", err, tx.writes, tx.committed, tx.rolledBack)
	}
}
func TestSaveLoginTransactionAtomicity(t *testing.T) {
	for _, failure := range []string{"", "device", "commit", "disabled"} {
		t.Run(failure, func(t *testing.T) {
			r, tx, in := loginFixture()
			tx.rejectDevice = failure == "device"
			tx.failCommit = failure == "commit"
			tx.noRow = failure == "disabled"
			got, err := r.SaveLogin(context.Background(), in)
			if failure == "" {
				if err != nil || got.UserID != in.UserID || !tx.committed || tx.commitCalls != 1 || r.runner.(*loginFakeRunner).calls != 1 || strings.Join(tx.writes, ",") != "rotate,session,device" {
					t.Fatalf("transaction failed: %v %#v", err, tx)
				}
			} else {
				want := login.ErrDatabase
				if failure == "disabled" {
					want = login.ErrSession
				}
				if !errors.Is(err, want) || got.ID != "" || tx.committed || !tx.rolledBack {
					t.Fatalf("unsafe failure: %v %#v", err, tx)
				}
				if failure == "disabled" && strings.Join(tx.writes, ",") != "rotate,session" {
					t.Fatal("disabled user inserted device")
				}
			}
		})
	}
}

func TestRunnerAndCloseErrorsAreRedacted(t *testing.T) {
	r, tx, in := loginFixture()
	tx.failCommit = true
	if result, err := r.SaveLogin(context.Background(), in); result.ID != "" || err != login.ErrDatabase {
		t.Fatal("runner commit details escaped error boundary")
	}
	r.runner.(*loginFakeRunner).closeErr = errors.New("private close details")
	if err := r.Close(context.Background()); err != login.ErrDatabase {
		t.Fatal("runner close details escaped error boundary")
	}
}
func TestConnectionQueueCancellation(t *testing.T) {
	r, _, _ := loginFixture()
	r.gate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := r.enter(ctx); !errors.Is(err, login.ErrDatabase) {
		t.Fatal("busy connection ignored deadline")
	}
	r.leave()
}
func TestAuthConnectionErrorsRedacted(t *testing.T) {
	for _, dsn := range []string{"postgres://user:private-sentinel@[invalid", "postgres://user:private-sentinel@127.0.0.1:1/missing?sslmode=disable"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r, err := OpenLogin(ctx, dsn, time.Minute)
		if r != nil || !errors.Is(err, login.ErrDatabase) || strings.Contains(err.Error(), "private-sentinel") {
			t.Fatal("connection error escaped boundary")
		}
	}
}

// Counter writes expose no caller clock and preserve generated argument shape.
type counterTx struct {
	loginFakeTx
	calls []string
	sizes []int
}

func (c *counterTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.calls = append(c.calls, sql)
	c.sizes = append(c.sizes, len(args))
	return pgconn.NewCommandTag("SELECT 1"), nil
}
func TestCounterWritesHaveNoCallerClock(t *testing.T) {
	c := &counterTx{}
	runner := &loginFakeRunner{tx: c}
	r := &LoginRepository{runner: runner, gate: make(chan struct{}, 1)}
	if err := r.RecordFailure(context.Background(), "missing@example.test", netip.MustParseAddr("192.0.2.9")); err != nil {
		t.Fatal(err)
	}
	if err := r.FailDevice(context.Background(), "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"); err != nil {
		t.Fatal(err)
	}
	if len(c.sizes) != 2 || c.sizes[0] != 2 || c.sizes[1] != 1 {
		t.Fatalf("counter argument counts = %v", c.sizes)
	}
	if !strings.Contains(c.calls[0], "auth_record_failure") || !strings.Contains(c.calls[1], "auth_fail_known_device") {
		t.Fatal("counter writes bypassed server functions")
	}
	if runner.calls != 2 {
		t.Fatal("counter work bypassed transaction runner")
	}
}
