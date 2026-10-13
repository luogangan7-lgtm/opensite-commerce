// Responsibility: private staff-auth persistence through generated SQL.
// Dependencies: tenanttx identity runner and internal/db; identity login service calls.
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
	identityadapter "github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type authRunner interface {
	InMaintenance(context.Context, func(context.Context, pgx.Tx) error) error
	Close(context.Context) error
}

// LoginRepository owns one dedicated connection. Waiting for it observes the
// caller's deadline so authentication cannot create an unbounded lock queue.
type LoginRepository struct {
	runner authRunner
	gate   chan struct{}
	idle   time.Duration
}

var _ login.Repository = (*LoginRepository)(nil)

func OpenLogin(ctx context.Context, dsn string, idleTimeout time.Duration) (*LoginRepository, error) {
	if idleTimeout < time.Second {
		return nil, login.ErrDatabase
	}
	runner, err := identityadapter.OpenIdentity(ctx, dsn)
	if err != nil {
		return nil, login.ErrDatabase
	}
	r := &LoginRepository{runner: runner, gate: make(chan struct{}, 1), idle: idleTimeout}
	err = r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		allowed, err := q.ValidateAuthConnection(ctx)
		if err != nil || !allowed.Valid || !allowed.Bool {
			return login.ErrDatabase
		}
		return nil
	})
	if err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runner.Close(closeCtx)
		return nil, login.ErrDatabase
	}
	return r, nil
}

// run holds the deadline-aware connection gate through callback and commit.
// Only sqlc queries constructed from the current transaction reach callers.
func (r *LoginRepository) run(ctx context.Context, fn func(context.Context, *db.Queries) error) error {
	if err := r.enter(ctx); err != nil {
		return err
	}
	defer r.leave()
	err := r.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, db.New(tx)) })
	if err == nil || errors.Is(err, login.ErrNotFound) || errors.Is(err, login.ErrSession) {
		return err
	}
	return login.ErrDatabase
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
	if err := r.runner.Close(ctx); err != nil {
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
	var result login.Credential
	err := r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		row, err := q.LoginCredential(ctx, identifier)
		if errors.Is(err, pgx.ErrNoRows) {
			return login.ErrNotFound
		}
		if err != nil {
			return err
		}
		result = login.Credential{UserID: row.ID.String(), PasswordHash: row.PasswordHash, Status: row.Status}
		return nil
	})
	if err != nil {
		return login.Credential{}, err
	}
	return result, nil
}
func (r *LoginRepository) Cooldowns(ctx context.Context, identifier string, ip netip.Addr) (time.Time, time.Time, error) {
	if !ip.IsValid() {
		return time.Time{}, time.Time{}, login.ErrDatabase
	}
	var row db.LoginCooldownsRow
	err := r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		var err error
		row, err = q.LoginCooldowns(ctx, db.LoginCooldownsParams{Identifier: identifier, Ip: ip.Unmap()})
		return err
	})
	if err != nil {
		return time.Time{}, time.Time{}, login.ErrDatabase
	}
	return row.AccountUntil.Time, row.IpUntil.Time, nil
}
func (r *LoginRepository) RecordFailure(ctx context.Context, identifier string, ip netip.Addr) error {
	if !ip.IsValid() {
		return login.ErrDatabase
	}
	return r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		return q.LoginRecordFailure(ctx, db.LoginRecordFailureParams{Identifier: identifier, Ip: ip.Unmap()})
	})
}
func (r *LoginRepository) Device(ctx context.Context, hash [32]byte) (login.Device, error) {
	var row db.LoginDeviceRow
	err := r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		var err error
		row, err = q.LoginDevice(ctx, hash[:])
		if errors.Is(err, pgx.ErrNoRows) {
			return login.ErrNotFound
		}
		return err
	})
	if err != nil {
		return login.Device{}, err
	}
	return login.Device{ID: row.ID.String(), UserID: row.UserID.String(), ExpiresAt: row.ExpiresAt.Time, Revoked: row.RevokedAt.Valid}, nil
}
func (r *LoginRepository) FailDevice(ctx context.Context, id string) error {
	u, err := uuid(id)
	if err != nil {
		return login.ErrDatabase
	}
	return r.run(ctx, func(ctx context.Context, q *db.Queries) error { return q.LoginFailDevice(ctx, u) })
}
func (r *LoginRepository) SaveLogin(ctx context.Context, in login.SessionInput) (login.Session, error) {
	u, err := uuid(in.UserID)
	if err != nil || in.CreatedAt.IsZero() || !in.ExpiresAt.After(in.CreatedAt) || !in.DeviceExpiresAt.After(in.CreatedAt) {
		return login.Session{}, login.ErrDatabase
	}
	var result login.Session
	err = r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		if in.OldTokenHash != nil {
			n, err := q.LoginRotateOldSession(ctx, db.LoginRotateOldSessionParams{TokenHash: in.OldTokenHash[:], UserID: u, IdleSeconds: int64(r.idle / time.Second)})
			if err != nil {
				return err
			}
			if n != 1 {
				return login.ErrSession
			}
		}
		row, err := q.LoginInsertSession(ctx, db.LoginInsertSessionParams{UserID: u, TokenHash: in.TokenHash[:], CreatedAt: stamp(in.CreatedAt), ExpiresAt: stamp(in.ExpiresAt)})
		if errors.Is(err, pgx.ErrNoRows) {
			return login.ErrSession
		}
		if err != nil {
			return err
		}
		if err = q.LoginInsertDevice(ctx, db.LoginInsertDeviceParams{UserID: u, CookieHash: in.DeviceHash[:], CreatedAt: stamp(in.CreatedAt), ExpiresAt: stamp(in.DeviceExpiresAt)}); err != nil {
			return err
		}
		result = session(row.ID, row.UserID, row.CreatedAt, row.LastSeenAt, row.ExpiresAt)
		return nil
	})
	if err != nil {
		return login.Session{}, err
	}
	return result, nil
}
func (r *LoginRepository) LookupSession(ctx context.Context, hash [32]byte, at time.Time, idle time.Duration) (login.Session, error) {
	if idle < time.Second || at.IsZero() {
		return login.Session{}, login.ErrSession
	}
	var row db.LoginLookupSessionRow
	err := r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		var err error
		row, err = q.LoginLookupSession(ctx, db.LoginLookupSessionParams{At: stamp(at), TokenHash: hash[:], IdleSeconds: int64(idle / time.Second)})
		if errors.Is(err, pgx.ErrNoRows) {
			return login.ErrSession
		}
		return err
	})
	if err != nil {
		return login.Session{}, err
	}
	return session(row.ID, row.UserID, row.CreatedAt, row.LastSeenAt, row.ExpiresAt), nil
}
func (r *LoginRepository) RevokeSession(ctx context.Context, id string, at time.Time) error {
	u, err := uuid(id)
	if err != nil {
		return login.ErrSession
	}
	return r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		n, err := q.LoginRevokeSession(ctx, db.LoginRevokeSessionParams{At: stamp(at), ID: u})
		if err != nil {
			return err
		}
		if n != 1 {
			return login.ErrSession
		}
		return nil
	})
}
