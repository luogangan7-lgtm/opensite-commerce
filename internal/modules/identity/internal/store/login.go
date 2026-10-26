// Responsibility: private staff-auth persistence through generated SQL.
// Dependencies: tenanttx identity runner and internal/db; identity login service calls.
// Invariants: runner-admitted app_auth LOGIN; isolated bounded lanes; atomic rotation;
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
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/observability"
	identityadapter "github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type authRunner interface {
	RunAuthentication(context.Context, func(context.Context, *db.Queries) error) error
	Close(context.Context) error
}

// LoginRepository isolates session queries from credential work. Each runner
// owns one connection, queue, role validation and an internal two-second budget.
type LoginRepository struct {
	runner, sessionRunner authRunner
	idle                  time.Duration
}

var _ login.Repository = (*LoginRepository)(nil)

func OpenLogin(ctx context.Context, dsn string, idleTimeout time.Duration) (*LoginRepository, error) {
	return openLogin(ctx, dsn, idleTimeout, func(ctx context.Context, dsn string) (authRunner, error) {
		return identityadapter.OpenStaffAuthentication(ctx, dsn)
	})
}

// The private factory lets tests hold an acquired credential callback without
// competing with the production SQL lock timeout. Production always opens twice.
func openLogin(ctx context.Context, dsn string, idleTimeout time.Duration, open func(context.Context, string) (authRunner, error)) (*LoginRepository, error) {
	if idleTimeout < time.Second {
		return nil, login.ErrDatabase
	}
	runner, err := open(ctx, dsn)
	if err != nil {
		return nil, login.ErrDatabase
	}
	sessions, err := open(ctx, dsn)
	if err != nil {
		_ = runner.Close(ctx)
		return nil, login.ErrDatabase
	}
	r := &LoginRepository{runner: runner, sessionRunner: sessions, idle: idleTimeout}
	noop := func(context.Context, *db.Queries) error { return nil }
	if r.run(ctx, noop) != nil || r.runLane(ctx, sessions, noop) != nil {
		_ = r.Close(ctx)
		return nil, login.ErrDatabase
	}
	return r, nil
}

// run delegates all queue/deadline/role policy to the query-only adapter port.
func (r *LoginRepository) run(ctx context.Context, fn func(context.Context, *db.Queries) error) error {
	return r.runLane(observability.WithAuthenticationLane(ctx, "staff_credential"), r.runner, fn)
}
func (r *LoginRepository) runLane(ctx context.Context, runner authRunner, fn func(context.Context, *db.Queries) error) error {
	err := runner.RunAuthentication(ctx, fn)
	if err == nil || errors.Is(err, login.ErrNotFound) || errors.Is(err, login.ErrSession) || errors.Is(err, login.ErrCredentials) {
		return err
	}
	return login.ErrDatabase
}
func (r *LoginRepository) Close(ctx context.Context) error {
	credentialErr, sessionErr := r.runner.Close(ctx), r.sessionRunner.Close(ctx)
	if credentialErr != nil || sessionErr != nil {
		return login.ErrDatabase
	}
	return nil
}
func uuid(s string) (pgtype.UUID, error)   { var u pgtype.UUID; err := u.Scan(s); return u, err }
func stamp(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
func session(id, user pgtype.UUID, created, last, expires, databaseNow pgtype.Timestamptz) login.Session {
	return login.Session{DatabaseNow: databaseNow.Time, ID: id.String(), UserID: user.String(), CreatedAt: created.Time, LastSeenAt: last.Time, ExpiresAt: expires.Time}
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
func (r *LoginRepository) RotationSession(ctx context.Context, hash [32]byte, userID string) (bool, error) {
	u, err := uuid(userID)
	if err != nil {
		return false, login.ErrDatabase
	}
	var allowed bool
	err = r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		var err error
		allowed, err = q.LoginRotationSession(ctx, db.LoginRotationSessionParams{TokenHash: hash[:], UserID: u, IdleSeconds: int64(r.idle / time.Second)})
		return err
	})
	return allowed, err
}
func (r *LoginRepository) SaveLogin(ctx context.Context, in login.SessionInput) (login.Session, error) {
	u, err := uuid(in.UserID)
	if err != nil || in.CreatedAt.IsZero() || !in.ExpiresAt.After(in.CreatedAt) || !in.DeviceExpiresAt.After(in.CreatedAt) {
		return login.Session{}, login.ErrDatabase
	}
	var result login.Session
	err = r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		// Reset already owns the user row before touching sessions. Match its
		// lock order, then compare the PHC after any concurrent writer commits.
		if _, err := q.LoginLockUser(ctx, db.LoginLockUserParams{UserID: u, VerifiedPasswordHash: in.VerifiedPasswordHash}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return login.ErrCredentials
			}
			return err
		}
		if in.OldTokenHash != nil {
			if _, err := q.LoginLockSession(ctx, in.OldTokenHash[:]); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return login.ErrSession
				}
				return err
			}
			n, err := q.LoginRotateOldSession(ctx, db.LoginRotateOldSessionParams{TokenHash: in.OldTokenHash[:], UserID: u, IdleSeconds: int64(r.idle / time.Second)})
			if err != nil {
				return err
			}
			if n != 1 {
				return login.ErrSession
			}
		}
		row, err := q.LoginInsertSession(ctx, db.LoginInsertSessionParams{UserID: u, VerifiedPasswordHash: in.VerifiedPasswordHash, TokenHash: in.TokenHash[:], CreatedAt: stamp(in.CreatedAt), ExpiresAt: stamp(in.ExpiresAt)})
		if errors.Is(err, pgx.ErrNoRows) {
			return login.ErrSession
		}
		if err != nil {
			return err
		}
		if err = q.LoginInsertDevice(ctx, db.LoginInsertDeviceParams{UserID: u, CookieHash: in.DeviceHash[:], CreatedAt: stamp(in.CreatedAt), ExpiresAt: stamp(in.DeviceExpiresAt)}); err != nil {
			return err
		}
		result = session(row.ID, row.UserID, row.CreatedAt, row.LastSeenAt, row.ExpiresAt, row.DatabaseNow)
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
	err := r.runLane(observability.WithAuthenticationLane(ctx, "staff_session"), r.sessionRunner, func(ctx context.Context, q *db.Queries) error {
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
	return session(row.ID, row.UserID, row.CreatedAt, row.LastSeenAt, row.ExpiresAt, row.DatabaseNow), nil
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
