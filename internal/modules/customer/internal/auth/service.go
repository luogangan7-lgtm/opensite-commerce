// Responsibilities: tenant-bound registration, mailbox proof, password resets
// and session admission. Dependencies: public repository/mailer and bounded KDF;
// module/HTTP invoke this port. Invariants: no identity before email proof,
// no database mutation after KDF deadline, and old credential snapshots cannot
// create sessions after reset. Repository transactions own atomic consume/CAS.
package auth

import (
	"context"
	"errors"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
	"net/mail"
	"net/netip"
	"strings"
	"unicode"
	"unicode/utf8"
)

func validEmail(email string) bool {
	a, e := mail.ParseAddress(email)
	at := strings.LastIndexByte(email, '@')
	return e == nil && a.Address == email && len(email) <= 254 && at > 0 && strings.Contains(email[at+1:], ".") && utf8.ValidString(email) && strings.IndexFunc(email, unicode.IsControl) < 0
}
func normalizedEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }
func validIP(ip netip.Addr) bool          { return ip.IsValid() && ip.Zone() == "" }
func validPassword(password string, minimum int) bool {
	for _, r := range password {
		if r == 0 || r == '\r' || r == '\n' {
			return false
		}
	}
	return len(password) >= minimum && len(password) <= 1024 && utf8.ValidString(password)
}
func input(tenant, email, password string, minimum int, ip netip.Addr) bool {
	return tenanttx.ValidTenantID(tenant) && validEmail(email) && validPassword(password, minimum) && validIP(ip)
}

func (s *Service) Register(ctx context.Context, tenant, email, password string, ip netip.Addr) error {
	email = normalizedEmail(email)
	if !input(tenant, email, password, 12, ip) {
		return c.ErrInvalidInput
	}
	ctx, finish, err := s.begin(ctx, tenant, email, ip)
	if err != nil {
		return err
	}
	defer finish()
	phc, err := s.hashPassword(ctx, password)
	if err != nil {
		return err
	}
	raw, hash, err := newToken()
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return rateLimited(0)
	}
	if err = s.cfg.Repo.PrepareRegistration(ctx, tenant, email, phc, hash, s.cfg.Now().Add(s.cfg.ChallengeTTL)); err != nil {
		return c.ErrUnavailable
	}
	if ctx.Err() != nil {
		return rateLimited(0)
	}
	if err = s.cfg.Mailer.Send(ctx, tenant, email, "register", raw); err != nil {
		return c.ErrUnavailable
	}
	return nil
}
func (s *Service) Verify(ctx context.Context, tenant, token string) error {
	hash, ok := tokenHash(token)
	if !tenanttx.ValidTenantID(tenant) || !ok {
		return c.ErrToken
	}
	if ctx.Err() != nil {
		return c.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.HashTimeout)
	defer cancel()
	if err := s.cfg.Repo.ConsumeVerification(ctx, tenant, hash); err != nil {
		if errors.Is(err, c.ErrToken) || errors.Is(err, c.ErrNotFound) {
			return c.ErrToken
		}
		return c.ErrUnavailable
	}
	return nil
}
func (s *Service) RequestReset(ctx context.Context, tenant, email string, ip netip.Addr) error {
	email = normalizedEmail(email)
	if !tenanttx.ValidTenantID(tenant) || !validEmail(email) || !ip.IsValid() || ip.Zone() != "" {
		return c.ErrInvalidInput
	}
	ctx, finish, err := s.begin(ctx, tenant, email, ip)
	if err != nil {
		return err
	}
	defer finish()
	// Eligibility belongs to the atomic repository command. Every valid address
	// takes the same bounded mailbox envelope path; unknown, disabled or
	// unverified accounts receive an unredeemable random token. A local transport
	// failure therefore has the same public response for every account state.
	raw, hash, err := newToken()
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return rateLimited(0)
	}
	if err = s.cfg.Repo.PrepareReset(ctx, tenant, email, hash, s.cfg.Now().Add(s.cfg.ChallengeTTL)); err != nil {
		return c.ErrUnavailable
	}
	if ctx.Err() != nil {
		return rateLimited(0)
	}
	if err = s.cfg.Mailer.Send(ctx, tenant, email, "reset", raw); err != nil {
		return c.ErrUnavailable
	}
	return nil
}
func (s *Service) Reset(ctx context.Context, tenant, token, password string, ip netip.Addr) error {
	hash, ok := tokenHash(token)
	if !tenanttx.ValidTenantID(tenant) || !ok {
		return c.ErrToken
	}
	if !validPassword(password, 12) || !validIP(ip) {
		return c.ErrInvalidInput
	}
	// Before atomic consume, a challenge has no email. It receives an opaque
	// per-challenge account bucket plus the global and shared IP budgets.
	ctx, finish, err := s.begin(ctx, tenant, token, ip)
	if err != nil {
		return err
	}
	defer finish()
	phc, err := s.hashPassword(ctx, password)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return rateLimited(0)
	}
	if err = s.cfg.Repo.ConsumeReset(ctx, tenant, hash, phc); err != nil {
		if errors.Is(err, c.ErrToken) || errors.Is(err, c.ErrNotFound) {
			return c.ErrToken
		}
		return c.ErrUnavailable
	}
	return nil
}
func (s *Service) Login(ctx context.Context, tenant, email, password string, ip netip.Addr, previousToken string) (c.Session, error) {
	email = normalizedEmail(email)
	if !input(tenant, email, password, 1, ip) {
		return c.Session{}, c.ErrInvalidInput
	}
	var oldHash *[32]byte
	if previousToken != "" {
		hash, ok := tokenHash(previousToken)
		if !ok {
			return c.Session{}, c.ErrUnauthorized
		}
		oldHash = &hash
	}
	ctx, finish, err := s.begin(ctx, tenant, email, ip)
	if err != nil {
		return c.Session{}, err
	}
	defer finish()
	credential, err := s.cfg.Repo.Credential(ctx, tenant, email)
	if err != nil && !errors.Is(err, c.ErrNotFound) {
		return c.Session{}, c.ErrUnavailable
	}
	eligible := err == nil && credential.Active && credential.Verified && tenanttx.ValidTenantID(credential.CustomerID) && normalizedEmail(credential.Email) == email
	matched, err := s.verifyPassword(ctx, password, credential.Hash, eligible)
	if err != nil {
		return c.Session{}, err
	}
	if !matched {
		account, ipHash := s.rateHashes(tenant, email, ip)
		if ctx.Err() != nil {
			return c.Session{}, rateLimited(0)
		}
		if err = s.cfg.Repo.Failed(ctx, tenant, account, ipHash); err != nil {
			return c.Session{}, c.ErrUnavailable
		}
		return c.Session{}, c.ErrInvalidCredentials
	}
	raw, hash, err := newToken()
	if err != nil {
		return c.Session{}, err
	}
	if ctx.Err() != nil {
		return c.Session{}, rateLimited(0)
	}
	now := s.cfg.Now()
	session, err := s.cfg.Repo.SaveSession(ctx, tenant, credential.CustomerID, credential.Hash, oldHash, hash, now.Add(s.cfg.SessionTTL), now.Add(s.cfg.IdleTTL))
	if err != nil {
		if errors.Is(err, c.ErrUnauthorized) || errors.Is(err, c.ErrNotFound) {
			return c.Session{}, c.ErrUnauthorized
		}
		return c.Session{}, c.ErrUnavailable
	}
	session.Email = normalizedEmail(session.Email)
	if !s.validSession(session, tenant) || session.CustomerID != credential.CustomerID {
		return c.Session{}, c.ErrUnauthorized
	}
	session.Token = raw
	return session, nil
}
func (s *Service) Current(ctx context.Context, tenant, token string) (c.Session, error) {
	hash, ok := tokenHash(token)
	if !tenanttx.ValidTenantID(tenant) || !ok {
		return c.Session{}, c.ErrUnauthorized
	}
	if ctx.Err() != nil {
		return c.Session{}, c.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.HashTimeout)
	defer cancel()
	session, err := s.cfg.Repo.Session(ctx, tenant, hash, s.cfg.IdleTTL)
	if err != nil {
		if errors.Is(err, c.ErrUnauthorized) || errors.Is(err, c.ErrNotFound) {
			return c.Session{}, c.ErrUnauthorized
		}
		return c.Session{}, c.ErrUnavailable
	}
	session.Email = normalizedEmail(session.Email)
	if !s.validSession(session, tenant) {
		return c.Session{}, c.ErrUnauthorized
	}
	session.Token = ""
	return session, nil
}
func (s *Service) validSession(session c.Session, tenant string) bool {
	now := s.cfg.Now()
	return tenanttx.ValidTenantID(session.ID) && tenanttx.ValidTenantID(session.CustomerID) && session.TenantID == tenant && validEmail(session.Email) && session.ExpiresAt.After(now) && session.IdleExpiresAt.After(now) && !session.IdleExpiresAt.After(session.ExpiresAt)
}
func (s *Service) Logout(ctx context.Context, tenant, token string) error {
	hash, ok := tokenHash(token)
	if !tenanttx.ValidTenantID(tenant) || !ok {
		return c.ErrUnauthorized
	}
	if ctx.Err() != nil {
		return c.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.HashTimeout)
	defer cancel()
	if err := s.cfg.Repo.RevokeSession(ctx, tenant, hash); err != nil {
		if errors.Is(err, c.ErrUnauthorized) || errors.Is(err, c.ErrNotFound) {
			return c.ErrUnauthorized
		}
		return c.ErrUnavailable
	}
	return nil
}
