// Responsibility: test private persistence transaction and error boundaries.
// Dependencies: generated db queries with a pgx transaction double.
// Callers: focused Go gate; invariants: lost rotation writes no successor,
// failed device insert rolls back, commit failure leaks no driver detail.
package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
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
	if tx.failCommit {
		return errors.New("private commit details")
	}
	tx.committed = true
	return nil
}
func (tx *loginFakeTx) Rollback(context.Context) error { tx.rolledBack = true; return nil }

type loginFakeConn struct {
	db.DBTX
	tx *loginFakeTx
}

func (c *loginFakeConn) Begin(context.Context) (pgx.Tx, error) { return c.tx, nil }
func (c *loginFakeConn) Close(context.Context) error           { return nil }
func loginFixture() (*LoginRepository, *loginFakeTx, login.SessionInput) {
	at := time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
	u, _ := uuid("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	tx := &loginFakeTx{user: u, at: at}
	conn := &loginFakeConn{tx: tx}
	r := &LoginRepository{conn: conn, queries: db.New(conn), gate: make(chan struct{}, 1), idle: 30 * time.Minute}
	old := [32]byte{1}
	in := login.SessionInput{UserID: u.String(), TokenHash: [32]byte{2}, DeviceHash: [32]byte{3}, CreatedAt: at, ExpiresAt: at.Add(time.Hour), DeviceExpiresAt: at.Add(24 * time.Hour), OldTokenHash: &old}
	return r, tx, in
}
func TestLostRotationNeverInsertsSuccessor(t *testing.T) {
	r, tx, in := loginFixture()
	tx.rotated = true
	_, err := r.SaveLogin(context.Background(), in)
	if !errors.Is(err, login.ErrSession) || strings.Join(tx.writes, ",") != "rotate" || tx.committed || !tx.rolledBack {
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
				if err != nil || got.UserID != in.UserID || !tx.committed || strings.Join(tx.writes, ",") != "rotate,session,device" {
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
