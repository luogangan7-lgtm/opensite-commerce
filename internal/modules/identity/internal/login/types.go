// Package login owns staff authentication and opaque session admission.
// It depends on the identity repository port and clock; HTTP transport calls it.
// Identity secrets stay in the auth database; private fields identify trusted actors.
package login

import (
	"context"
	"errors"
	"net/netip"
	"time"
)

var (
	ErrNotFound    = errors.New("identity record not found")
	ErrCredentials = errors.New("invalid credentials")
	ErrRateLimited = errors.New("authentication rate limited")
	ErrSession     = errors.New("invalid session")
	ErrDatabase    = errors.New("authentication unavailable")
)

type Credential struct{ UserID, PasswordHash, Status string }
type Device struct {
	ID, UserID string
	ExpiresAt  time.Time
	Revoked    bool
}
type Session struct {
	ID, UserID                       string
	CreatedAt, LastSeenAt, ExpiresAt time.Time
}
type SessionInput struct {
	UserID                                string
	TokenHash, DeviceHash                 [32]byte
	CreatedAt, ExpiresAt, DeviceExpiresAt time.Time
	OldTokenHash                          *[32]byte
}

// Repository is implemented by the private sqlc store. SaveLogin must revoke the
// matching old session conditionally before inserting a successor in one tx.
type Repository interface {
	Credential(context.Context, string) (Credential, error)
	Cooldowns(context.Context, string, netip.Addr) (time.Time, time.Time, error)
	RecordFailure(context.Context, string, netip.Addr) error
	Device(context.Context, [32]byte) (Device, error)
	FailDevice(context.Context, string) error
	SaveLogin(context.Context, SessionInput) (Session, error)
	LookupSession(context.Context, [32]byte, time.Time, time.Duration) (Session, error)
	RevokeSession(context.Context, string, time.Time) error
}

// Actor can be read externally but constructed only after session admission.
type Actor struct{ userID, sessionID string }

func (a Actor) UserID() string    { return a.userID }
func (a Actor) SessionID() string { return a.sessionID }

type actorKey struct{}

func ActorFromContext(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok && a.userID != "" && a.sessionID != ""
}
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

type Config struct {
	Origin                                       string
	SigningKey                                   []byte
	IdleTimeout, AbsoluteTimeout, DeviceLifetime time.Duration
	// HashTimeout bounds admission/database work and forbids writes after KDF deadline.
	// A bounded worker retains its slot until Argon2 completes; it cannot be preempted.
	HashTimeout                     time.Duration
	TrustedProxies                  []netip.Prefix
	HashSlots, KnownDeviceHashSlots int
	IPBurst, GlobalBurst            int
	Now                             func() time.Time
	// Deprecated: cooldowns reject immediately. Kept for config source compatibility.
	Wait func(context.Context, time.Duration) error
}
type LoginInput struct {
	Identifier, Password, DeviceToken, OldSessionToken string
	IP                                                 netip.Addr
}
type LoginResult struct {
	SessionToken, DeviceToken, CSRFToken string
	Session                              Session
}
