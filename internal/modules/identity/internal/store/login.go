// Responsibility: private staff-auth persistence through generated SQL.
// Dependencies: platform identity connection adapter and internal/db; callers: identity login service.
// Invariants: dedicated app_auth LOGIN; serialized connection; atomic rotation;
// driver errors and credentials never escape this boundary.
package store

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
	identityadapter "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/identitydb/pgxadapter"
)

type authConnection interface {
	db.DBTX
	Begin(context.Context) (pgx.Tx, error)
	Close(context.Context) error
}

// LoginRepository owns one dedicated connection. Waiting for it observes the
// caller's deadline so authentication cannot create an unbounded lock queue.
type LoginRepository struct {
	conn    authConnection
	queries *db.Queries
	gate    chan struct{}
	idle    time.Duration
}

var _ login.Repository = (*LoginRepository)(nil)

func OpenLogin(ctx context.Context, dsn string, idleTimeout time.Duration) (*LoginRepository, error) {
	if idleTimeout < time.Second {
		return nil, login.ErrDatabase
	}
	conn, err := identityadapter.Open(ctx, dsn)
	if err != nil {
		return nil, login.ErrDatabase
	}
	q := db.New(conn)
	allowed, err := q.ValidateAuthConnection(ctx)
	if err != nil || !allowed.Valid || !allowed.Bool {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
		return nil, login.ErrDatabase
	}
	return &LoginRepository{conn: conn, queries: q, gate: make(chan struct{}, 1), idle: idleTimeout}, nil
}
func (r *LoginRepository) enter(ctx context.Context) error {
	if ctx.Err() != nil {
		return login.ErrDatabase
	}
	select {
	case r.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return login.ErrDatabase
	}
}
func (r *LoginRepository) leave() { <-r.gate }
func (r *LoginRepository) Close(ctx context.Context) error {
	if err := r.enter(ctx); err != nil {
		return err
	}
	defer r.leave()
	if err := r.conn.Close(ctx); err != nil {
		return login.ErrDatabase
	}
	return nil
}
func uuid(s string) (pgtype.UUID, error)   { var u pgtype.UUID; err := u.Scan(s); return u, err }
func stamp(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
func session(id, user pgtype.UUID, created, last, expires pgtype.Timestamptz) login.Session {
	return login.Session{ID: id.String(), UserID: user.String(), CreatedAt: created.Time, LastSeenAt: last.Time, ExpiresAt: expires.Time}
}
func (r *LoginRepository) Credential(ctx context.Context, identifier string) (login.Credential, error) {
	if err := r.enter(ctx); err != nil {
		return login.Credential{}, err
	}
	defer r.leave()
	row, err := r.queries.LoginCredential(ctx, identifier)
	if errors.Is(err, pgx.ErrNoRows) {
		return login.Credential{}, login.ErrNotFound
	}
	if err != nil {
		return login.Credential{}, login.ErrDatabase
	}
	return login.Credential{UserID: row.ID.String(), PasswordHash: row.PasswordHash, Status: row.Status}, nil
}
func (r *LoginRepository) Cooldowns(ctx context.Context, identifier string, ip netip.Addr) (time.Time, time.Time, error) {
	if !ip.IsValid() {
		return time.Time{}, time.Time{}, login.ErrDatabase
	}
	if err := r.enter(ctx); err != nil {
		return time.Time{}, time.Time{}, err
	}
	defer r.leave()
	row, err := r.queries.LoginCooldowns(ctx, db.LoginCooldownsParams{Identifier: identifier, Ip: ip.Unmap()})
	if err != nil {
		return time.Time{}, time.Time{}, login.ErrDatabase
	}
	return row.AccountUntil.Time, row.IpUntil.Time, nil
}
func (r *LoginRepository) RecordFailure(ctx context.Context, identifier string, ip netip.Addr) error {
	if !ip.IsValid() {
		return login.ErrDatabase
	}
	if err := r.enter(ctx); err != nil {
		return err
	}
	defer r.leave()
	err := r.queries.LoginRecordFailure(ctx, db.LoginRecordFailureParams{Identifier: identifier, Ip: ip.Unmap()})
	if err != nil {
		return login.ErrDatabase
	}
	return nil
}
func (r *LoginRepository) Device(ctx context.Context, hash [32]byte) (login.Device, error) {
	if err := r.enter(ctx); err != nil {
		return login.Device{}, err
	}
	defer r.leave()
	row, err := r.queries.LoginDevice(ctx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return login.Device{}, login.ErrNotFound
	}
	if err != nil {
		return login.Device{}, login.ErrDatabase
	}
	return login.Device{ID: row.ID.String(), UserID: row.UserID.String(), ExpiresAt: row.ExpiresAt.Time, Revoked: row.RevokedAt.Valid}, nil
}
func (r *LoginRepository) FailDevice(ctx context.Context, id string) error {
	u, err := uuid(id)
	if err != nil {
		return login.ErrDatabase
	}
	if err = r.enter(ctx); err != nil {
		return err
	}
	defer r.leave()
	if err = r.queries.LoginFailDevice(ctx, u); err != nil {
		return login.ErrDatabase
	}
	return nil
}
func (r *LoginRepository) SaveLogin(ctx context.Context, in login.SessionInput) (login.Session, error) {
	u, err := uuid(in.UserID)
	if err != nil || in.CreatedAt.IsZero() || !in.ExpiresAt.After(in.CreatedAt) || !in.DeviceExpiresAt.After(in.CreatedAt) {
		return login.Session{}, login.ErrDatabase
	}
	if err = r.enter(ctx); err != nil {
		return login.Session{}, err
	}
	defer r.leave()
	tx, err := r.conn.Begin(ctx)
	if err != nil {
		return login.Session{}, login.ErrDatabase
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	q := r.queries.WithTx(tx)
	if in.OldTokenHash != nil {
		n, err := q.LoginRotateOldSession(ctx, db.LoginRotateOldSessionParams{TokenHash: in.OldTokenHash[:], UserID: u, IdleSeconds: int64(r.idle / time.Second)})
		if err != nil {
			return login.Session{}, login.ErrDatabase
		}
		if n != 1 {
			return login.Session{}, login.ErrSession
		}
	}
	row, err := q.LoginInsertSession(ctx, db.LoginInsertSessionParams{UserID: u, TokenHash: in.TokenHash[:], CreatedAt: stamp(in.CreatedAt), ExpiresAt: stamp(in.ExpiresAt)})
	if errors.Is(err, pgx.ErrNoRows) {
		return login.Session{}, login.ErrSession
	}
	if err != nil {
		return login.Session{}, login.ErrDatabase
	}
	if err = q.LoginInsertDevice(ctx, db.LoginInsertDeviceParams{UserID: u, CookieHash: in.DeviceHash[:], CreatedAt: stamp(in.CreatedAt), ExpiresAt: stamp(in.DeviceExpiresAt)}); err != nil {
		return login.Session{}, login.ErrDatabase
	}
	if err = tx.Commit(ctx); err != nil {
		return login.Session{}, login.ErrDatabase
	}
	return session(row.ID, row.UserID, row.CreatedAt, row.LastSeenAt, row.ExpiresAt), nil
}
func (r *LoginRepository) LookupSession(ctx context.Context, hash [32]byte, at time.Time, idle time.Duration) (login.Session, error) {
	if idle < time.Second || at.IsZero() {
		return login.Session{}, login.ErrSession
	}
	if err := r.enter(ctx); err != nil {
		return login.Session{}, err
	}
	defer r.leave()
	row, err := r.queries.LoginLookupSession(ctx, db.LoginLookupSessionParams{At: stamp(at), TokenHash: hash[:], IdleSeconds: int64(idle / time.Second)})
	if errors.Is(err, pgx.ErrNoRows) {
		return login.Session{}, login.ErrSession
	}
	if err != nil {
		return login.Session{}, login.ErrDatabase
	}
	return session(row.ID, row.UserID, row.CreatedAt, row.LastSeenAt, row.ExpiresAt), nil
}
func (r *LoginRepository) RevokeSession(ctx context.Context, id string, at time.Time) error {
	u, err := uuid(id)
	if err != nil {
		return login.ErrSession
	}
	if err = r.enter(ctx); err != nil {
		return err
	}
	defer r.leave()
	n, err := r.queries.LoginRevokeSession(ctx, db.LoginRevokeSessionParams{At: stamp(at), ID: u})
	if err != nil {
		return login.ErrDatabase
	}
	if n != 1 {
		return login.ErrSession
	}
	return nil
}
