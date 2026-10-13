// Package contract owns the public customer authentication and persistence ports.
// Dependencies: standard library; customer service, store and HTTP consume these
// ports. Invariants: tenant-bound buyers are independent of employees, secrets
// never enter JSON or persistence except as hashes, and reset revokes all sessions.
package contract

import (
	"context"
	"errors"
	"net/netip"
	"time"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrToken              = errors.New("invalid or expired token")
	ErrUnauthorized       = errors.New("authentication required")
	ErrRateLimited        = errors.New("authentication rate limited")
	ErrInvalidInput       = errors.New("invalid authentication input")
	ErrUnavailable        = errors.New("authentication unavailable")
	ErrNotFound           = errors.New("customer credential not found")
)

type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

type Session struct {
	ID            string    `json:"session_id"`
	TenantID      string    `json:"tenant_id"`
	CustomerID    string    `json:"customer_id"`
	Email         string    `json:"email"`
	Token         string    `json:"-"`
	ExpiresAt     time.Time `json:"expires_at"`
	IdleExpiresAt time.Time `json:"idle_expires_at"`
}

type Service interface {
	Register(context.Context, string, string, string, netip.Addr) error
	Verify(context.Context, string, string) error
	RequestReset(context.Context, string, string, netip.Addr) error
	Reset(context.Context, string, string, string, netip.Addr) error
	Login(context.Context, string, string, string, netip.Addr, string) (Session, error)
	Current(context.Context, string, string) (Session, error)
	Logout(context.Context, string, string) error
}

type Credential struct {
	CustomerID, Email, Hash string
	Verified, Active        bool
}

// Repository mutations must be atomic under a dedicated customer-auth tenant
// transaction. Challenges are one-use and hashed; verification may attach a
// credential to an existing guest only after ownership proof. ConsumeReset must
// revoke every customer session in the same transaction as the password change.
type Repository interface {
	Credential(context.Context, string, string) (Credential, error)
	PrepareRegistration(context.Context, string, string, string, [32]byte, time.Time) error
	ConsumeVerification(context.Context, string, [32]byte) error
	PrepareReset(context.Context, string, string, [32]byte, time.Time) error
	ConsumeReset(context.Context, string, [32]byte, string) error
	SaveSession(context.Context, string, string, string, *[32]byte, [32]byte, time.Time, time.Time) (Session, error)
	Session(context.Context, string, [32]byte, time.Duration) (Session, error)
	RevokeSession(context.Context, string, [32]byte) error
	Admit(context.Context, string, [32]byte, [32]byte, time.Time) (time.Duration, error)
	Failed(context.Context, string, [32]byte, [32]byte) error
}

// Mailer is an explicit, server-only delivery seam: tokens must not be logged,
// returned by the registration API or persisted in an outbox as plaintext.
type Mailer interface {
	Send(context.Context, string, string, string, string) error
}
