// Package store implements customer persistence using generated customer queries.
// Dependencies: db/sqlc, customer contract and pgx values; module.NewPostgres calls it.
// Invariants: caller-owned app_customer_auth tenant transactions only, no connection
// creation, raw tokens, credential DML or unredacted database errors. Customer SQL
// functions own credentials, proof tokens, sessions, limits and verified order links.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

// Run receives generated queries bound to one already-authorized tenant transaction.
type Run func(context.Context, string, func(*db.Queries) error) error

type Store struct{ run Run }

func New(run Run) *Store { return &Store{run: run} }

var _ c.Repository = (*Store)(nil)

func (s *Store) in(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.run == nil || !tenanttx.ValidTenantID(tenant) {
		return c.ErrUnavailable
	}
	err := s.run(ctx, tenant, func(q *db.Queries) error {
		if q == nil {
			return c.ErrUnavailable
		}
		return fn(q)
	})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, safe := range []error{c.ErrNotFound, c.ErrToken, c.ErrUnauthorized} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return c.ErrUnavailable
}

func uuid(value string) pgtype.UUID {
	var id pgtype.UUID
	_ = id.Scan(value)
	return id
}

func instant(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value.UTC(), Valid: !value.IsZero()}
}

func (s *Store) Credential(ctx context.Context, tenant, email string) (credential c.Credential, err error) {
	err = s.in(ctx, tenant, func(q *db.Queries) error {
		row, e := q.CustomerCredential(ctx, db.CustomerCredentialParams{TenantID: uuid(tenant), Email: email})
		if errors.Is(e, pgx.ErrNoRows) {
			return c.ErrNotFound
		}
		if e != nil {
			return e
		}
		if !row.CustomerID.Valid {
			return c.ErrUnavailable
		}
		credential = c.Credential{CustomerID: row.CustomerID.String(), Email: row.Email, Hash: row.PasswordHash, Active: row.Active, Verified: row.Verified}
		return nil
	})
	if err != nil {
		return c.Credential{}, err
	}
	return credential, nil
}

func (s *Store) PrepareRegistration(ctx context.Context, tenant, email, hash string, token [32]byte, ttl time.Duration) error {
	if ttl < time.Minute || ttl > time.Hour {
		return c.ErrUnavailable
	}
	return s.in(ctx, tenant, func(q *db.Queries) error {
		_, err := q.PrepareCustomerRegistration(ctx, db.PrepareCustomerRegistrationParams{Email: email, PendingHash: hash, TokenHash: token[:], ChallengeUs: int64(ttl / time.Microsecond)})
		return err
	})
}

func (s *Store) ConsumeVerification(ctx context.Context, tenant string, token [32]byte) error {
	return s.in(ctx, tenant, func(q *db.Queries) error {
		ok, err := q.ConsumeCustomerVerification(ctx, token[:])
		if err == nil && !ok {
			return c.ErrToken
		}
		return err
	})
}

func (s *Store) PrepareReset(ctx context.Context, tenant, email string, token [32]byte, ttl time.Duration) error {
	if ttl < time.Minute || ttl > time.Hour {
		return c.ErrUnavailable
	}
	return s.in(ctx, tenant, func(q *db.Queries) error {
		_, err := q.PrepareCustomerReset(ctx, db.PrepareCustomerResetParams{Email: email, TokenHash: token[:], ChallengeUs: int64(ttl / time.Microsecond)})
		return err
	})
}

func (s *Store) ConsumeReset(ctx context.Context, tenant string, token [32]byte, hash string) error {
	return s.in(ctx, tenant, func(q *db.Queries) error {
		ok, err := q.ConsumeCustomerReset(ctx, db.ConsumeCustomerResetParams{TokenHash: token[:], NewHash: hash})
		if err == nil && !ok {
			return c.ErrToken
		}
		return err
	})
}

func sessionDocument(document, tenant string) (c.Session, error) {
	if document == "" {
		return c.Session{}, c.ErrUnauthorized
	}
	if len(document) > 8192 {
		return c.Session{}, c.ErrUnavailable
	}
	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.DisallowUnknownFields()
	var session c.Session
	if err := decoder.Decode(&session); err != nil {
		return c.Session{}, c.ErrUnavailable
	}
	if decoder.Decode(&struct{}{}) != io.EOF || session.TenantID != tenant || !tenanttx.ValidTenantID(session.ID) || !tenanttx.ValidTenantID(session.CustomerID) || session.Email == "" || len(session.Email) > 254 || session.ExpiresAt.IsZero() || session.IdleExpiresAt.IsZero() || session.IdleExpiresAt.After(session.ExpiresAt) {
		return c.Session{}, c.ErrUnavailable
	}
	return session, nil
}

func (s *Store) SaveSession(ctx context.Context, tenant, customer, expectedHash string, old *[32]byte, next [32]byte, absolute, idle time.Duration) (session c.Session, err error) {
	if idle < time.Minute || idle > 24*time.Hour || absolute < idle || absolute > 7*24*time.Hour {
		return c.Session{}, c.ErrUnavailable
	}
	var oldHash []byte
	if old != nil {
		oldHash = old[:]
	}
	err = s.in(ctx, tenant, func(q *db.Queries) error {
		document, e := q.SaveCustomerSession(ctx, db.SaveCustomerSessionParams{CustomerID: uuid(customer), ExpectedHash: expectedHash, OldHash: oldHash, TokenHash: next[:], AbsoluteUs: int64(absolute / time.Microsecond), IdleUs: int64(idle / time.Microsecond)})
		if e != nil {
			return e
		}
		session, e = sessionDocument(document, tenant)
		return e
	})
	if err != nil {
		return c.Session{}, err
	}
	return session, nil
}

func (s *Store) Session(ctx context.Context, tenant string, token [32]byte, idle time.Duration) (session c.Session, err error) {
	if idle < time.Second || idle > 24*time.Hour {
		return c.Session{}, c.ErrUnavailable
	}
	err = s.in(ctx, tenant, func(q *db.Queries) error {
		document, e := q.GetCustomerSession(ctx, db.GetCustomerSessionParams{TokenHash: token[:], IdleSeconds: int64(idle / time.Second)})
		if e != nil {
			return e
		}
		session, e = sessionDocument(document, tenant)
		return e
	})
	if err != nil {
		return c.Session{}, err
	}
	return session, nil
}

func (s *Store) RevokeSession(ctx context.Context, tenant string, token [32]byte) error {
	return s.in(ctx, tenant, func(q *db.Queries) error {
		_, err := q.RevokeCustomerSession(ctx, token[:])
		return err
	})
}

func (s *Store) Admit(ctx context.Context, tenant string, account, ip [32]byte, now time.Time) (delay time.Duration, err error) {
	err = s.in(ctx, tenant, func(q *db.Queries) error {
		seconds, e := q.AdmitCustomerAuth(ctx, db.AdmitCustomerAuthParams{AccountHash: account[:], IpHash: ip[:], Now: instant(now)})
		if e != nil {
			return e
		}
		if seconds < 0 || seconds > 86400 {
			return c.ErrUnavailable
		}
		delay = time.Duration(seconds) * time.Second
		return nil
	})
	if err != nil {
		return 0, err
	}
	return delay, nil
}

func (s *Store) Failed(ctx context.Context, tenant string, account, ip [32]byte) error {
	return s.in(ctx, tenant, func(q *db.Queries) error {
		_, err := q.FailedCustomerAuth(ctx, db.FailedCustomerAuthParams{AccountHash: account[:], IpHash: ip[:]})
		return err
	})
}
