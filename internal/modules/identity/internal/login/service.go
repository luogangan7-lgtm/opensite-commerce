// Service owns password login, session admission and immediate cooldown rejection.
// Dependencies: Repository port and crypto helpers; called by identity HTTP transport.
// Invariants: no DB error escapes; rejected attempts never increment durable failures;
// only server-validated sessions create Actor; old-token replacement is atomic in SaveLogin.
// Ordinary arrivals cannot consume reserved candidate DB or IP-map capacity.
package login

import (
	"context"
	"errors"
	"net/mail"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

type ipWindow struct {
	start time.Time
	used  int
}
type Service struct {
	repo                           Repository
	cfg                            Config
	hashSlots, knownSlots          chan struct{}
	admissionSlots, candidateSlots chan struct{}
	mu                             sync.Mutex
	ips                            map[netip.Addr]ipWindow
	knownIPs                       map[netip.Addr]ipWindow
	global, knownGlobal            ipWindow
	deriveKey                      func([]byte, []byte, uint32, uint32, uint8, uint32) []byte
}

func New(repo Repository, cfg Config) (*Service, error) {
	u, e := url.Parse(cfg.Origin)
	if repo == nil || e != nil || u.Scheme != "https" || u.Hostname() == "" || strings.HasSuffix(u.Host, ":") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.String() != cfg.Origin || len(cfg.SigningKey) < 32 {
		return nil, ErrDatabase
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	if cfg.AbsoluteTimeout == 0 {
		cfg.AbsoluteTimeout = 12 * time.Hour
	}
	if cfg.DeviceLifetime == 0 {
		cfg.DeviceLifetime = 90 * 24 * time.Hour
	}
	if cfg.HashTimeout == 0 {
		cfg.HashTimeout = 5 * time.Second
	}
	if cfg.HashSlots == 0 {
		cfg.HashSlots = 2
	}
	if cfg.KnownDeviceHashSlots == 0 {
		cfg.KnownDeviceHashSlots = 1
	}
	if cfg.IPBurst == 0 {
		cfg.IPBurst = 30
	}
	if cfg.GlobalBurst == 0 {
		cfg.GlobalBurst = 300
	}
	if cfg.HashTimeout < time.Millisecond || cfg.HashTimeout > 30*time.Second || cfg.IdleTimeout < time.Minute || cfg.IdleTimeout > 24*time.Hour || cfg.AbsoluteTimeout < cfg.IdleTimeout || cfg.AbsoluteTimeout > 7*24*time.Hour || cfg.DeviceLifetime < time.Hour || cfg.DeviceLifetime > 365*24*time.Hour || cfg.HashSlots < 1 || cfg.KnownDeviceHashSlots < 1 || cfg.HashSlots+cfg.KnownDeviceHashSlots > 16 || cfg.IPBurst < 1 || cfg.IPBurst > 10000 || cfg.GlobalBurst < 2 || cfg.GlobalBurst > 100000 {
		return nil, ErrDatabase
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	for _, prefix := range cfg.TrustedProxies {
		if !prefix.IsValid() {
			return nil, ErrDatabase
		}
	}
	cfg.TrustedProxies = append([]netip.Prefix(nil), cfg.TrustedProxies...)
	cfg.SigningKey = append([]byte(nil), cfg.SigningKey...)
	return &Service{repo: repo, cfg: cfg, hashSlots: make(chan struct{}, cfg.HashSlots), knownSlots: make(chan struct{}, cfg.KnownDeviceHashSlots), admissionSlots: make(chan struct{}, cfg.HashSlots), candidateSlots: make(chan struct{}, cfg.KnownDeviceHashSlots), ips: make(map[netip.Addr]ipWindow), knownIPs: make(map[netip.Addr]ipWindow), deriveKey: argon2.IDKey}, nil
}

// ValidIdentifier is shared with schema admission to keep malformed identifiers out of DB work.
func ValidIdentifier(identifier string) bool {
	at := strings.LastIndexByte(identifier, '@')
	a, e := mail.ParseAddress(identifier)
	return e == nil && a.Address == identifier && len(identifier) <= 254 && at > 0 && strings.Contains(identifier[at+1:], ".") && !strings.ContainsAny(identifier, "\r\n\x00") && utf8.ValidString(identifier)
}
func NormalizeIdentifier(identifier string) string {
	return strings.ToLower(strings.TrimSpace(identifier))
}

func (s *Service) Login(ctx context.Context, in LoginInput) (LoginResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.HashTimeout)
	defer cancel()
	arrival := s.cfg.Now()
	in.Identifier = NormalizeIdentifier(in.Identifier)
	if !ValidIdentifier(in.Identifier) || len(in.Password) == 0 || len(in.Password) > 1024 || !utf8.ValidString(in.Password) || strings.ContainsAny(in.Password, "\r\n\x00") || !in.IP.IsValid() || ctx.Err() != nil {
		return LoginResult{}, ErrCredentials
	}
	in.IP = rateLimitIP(in.IP)
	// A MAC-valid, canonical device token is only a candidate. DB validation
	// below still decides account exemption and reserved Argon2 eligibility.
	var candidateHash [32]byte
	candidate := false
	if payload, ok := s.verified("device", in.DeviceToken); ok {
		if hash, err := tokenHash(payload); err == nil {
			candidateHash, candidate = hash, true
		}
	}
	if !s.admitIPClass(in.IP, arrival, candidate) || !s.admitGlobal(candidate, arrival) {
		return LoginResult{}, ErrRateLimited
	}
	admission := s.admissionSlots
	if candidate {
		admission = s.candidateSlots
	}
	select {
	case admission <- struct{}{}:
	default:
		return LoginResult{}, ErrRateLimited
	}
	// Bounds pre-KDF database admission only; cooldowns never sleep. Every return
	// releases admission if it has not already been released before hashing.
	defer func() {
		if admission != nil {
			<-admission
		}
	}()
	var oldHash *[32]byte
	if in.OldSessionToken != "" {
		h, e := tokenHash(in.OldSessionToken)
		if e != nil {
			return LoginResult{}, ErrSession
		}
		oldHash = &h
	}
	credential, e := s.repo.Credential(ctx, in.Identifier)
	if ctx.Err() != nil {
		return LoginResult{}, ErrRateLimited
	}
	if e != nil && !errors.Is(e, ErrNotFound) {
		return LoginResult{}, ErrDatabase
	}
	eligible := e == nil && credential.UserID != "" && credential.Status == "active"
	var device Device
	known := false
	if candidate {
		var err error
		device, err = s.repo.Device(ctx, candidateHash)
		if ctx.Err() != nil {
			return LoginResult{}, ErrRateLimited
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return LoginResult{}, ErrDatabase
		}
		known = err == nil && device.ID != "" && device.UserID == credential.UserID && !device.Revoked && device.ExpiresAt.After(arrival)
	}
	if candidate && !known {
		// A stale/wrong-user device must pay the ordinary global and in-flight
		// quota before cooldowns/password work; it cannot keep reserved capacity.
		if !s.admitGlobal(false, arrival) {
			return LoginResult{}, ErrRateLimited
		}
		select {
		case s.admissionSlots <- struct{}{}:
		default:
			return LoginResult{}, ErrRateLimited
		}
		<-admission
		admission = s.admissionSlots
	}
	account, accountIP, e := s.repo.Cooldowns(ctx, in.Identifier, in.IP)
	if ctx.Err() != nil {
		return LoginResult{}, ErrRateLimited
	}
	if e != nil {
		return LoginResult{}, ErrDatabase
	}
	if accountIP.After(arrival) {
		return LoginResult{}, ErrRateLimited
	}
	if !known && account.After(arrival) {
		return LoginResult{}, ErrRateLimited
	}
	if ctx.Err() != nil {
		return LoginResult{}, ErrRateLimited
	}
	// DB admission is distinct from KDF capacity; never retain it while hashing.
	<-admission
	admission = nil
	slots := s.hashSlots
	if known {
		slots = s.knownSlots
	}
	select {
	case slots <- struct{}{}:
	default:
		return LoginResult{}, ErrRateLimited
	}
	// IDKey cannot be preempted. The worker owns its slot until real completion;
	// deadline responses cannot release capacity for additional background KDFs.
	// The buffered result lets late completion exit without any repository writes.
	result := make(chan bool, 1)
	go func() {
		matched := s.verifyPassword(in.Password, credential.PasswordHash, eligible)
		<-slots
		result <- matched
	}()
	var verifiedPassword bool
	select {
	case verifiedPassword = <-result:
	case <-ctx.Done():
		return LoginResult{}, ErrRateLimited
	}
	if ctx.Err() != nil {
		return LoginResult{}, ErrRateLimited
	}
	if !verifiedPassword {
		now := s.cfg.Now()
		failureErr := s.repo.RecordFailure(ctx, in.Identifier, in.IP, now)
		var deviceErr error
		if known {
			deviceErr = s.repo.FailDevice(ctx, device.ID, now)
		}
		if failureErr != nil || deviceErr != nil {
			return LoginResult{}, ErrDatabase
		}
		return LoginResult{}, ErrCredentials
	}
	if ctx.Err() != nil {
		return LoginResult{}, ErrRateLimited
	}
	raw, e := newToken()
	if e != nil {
		return LoginResult{}, ErrDatabase
	}
	deviceRaw, e := newToken()
	if e != nil {
		return LoginResult{}, ErrDatabase
	}
	hash, _ := tokenHash(raw)
	dh, _ := tokenHash(deviceRaw)
	now := s.cfg.Now()
	session, e := s.repo.SaveLogin(ctx, SessionInput{UserID: credential.UserID, TokenHash: hash, DeviceHash: dh, CreatedAt: now, ExpiresAt: now.Add(s.cfg.AbsoluteTimeout), DeviceExpiresAt: now.Add(s.cfg.DeviceLifetime), OldTokenHash: oldHash})
	if e != nil {
		if errors.Is(e, ErrSession) {
			return LoginResult{}, ErrSession
		}
		return LoginResult{}, ErrDatabase
	}
	return LoginResult{SessionToken: raw, DeviceToken: s.sign("device", deviceRaw), CSRFToken: s.SessionCSRF(raw), Session: session}, nil
}

func (s *Service) Authenticate(ctx context.Context, rawToken string) (Actor, Session, error) {
	hash, e := tokenHash(rawToken)
	if e != nil {
		return Actor{}, Session{}, ErrSession
	}
	now := s.cfg.Now()
	session, e := s.repo.LookupSession(ctx, hash, now, s.cfg.IdleTimeout)
	if e != nil {
		if errors.Is(e, ErrNotFound) || errors.Is(e, ErrSession) {
			return Actor{}, Session{}, ErrSession
		}
		return Actor{}, Session{}, ErrDatabase
	}
	// A queued lookup may finish after another request advanced last_seen_at.
	// Validate the returned record against completion, not the earlier arrival.
	now = s.cfg.Now()
	if session.ID == "" || session.UserID == "" || !session.ExpiresAt.After(now) || session.CreatedAt.After(now) || session.LastSeenAt.After(now) || !session.CreatedAt.Add(s.cfg.AbsoluteTimeout).After(now) || !session.LastSeenAt.Add(s.cfg.IdleTimeout).After(now) {
		return Actor{}, Session{}, ErrSession
	}
	return Actor{userID: session.UserID, sessionID: session.ID}, session, nil
}
func (s *Service) Logout(ctx context.Context, a Actor) error {
	if a.userID == "" || a.sessionID == "" {
		return ErrSession
	}
	if e := s.repo.RevokeSession(ctx, a.sessionID, s.cfg.Now()); e != nil {
		return ErrDatabase
	}
	return nil
}

// IPv6 privacy addresses share a /64 in both in-memory and durable limits.
// IPv4 and IPv4-mapped addresses retain their individual IPv4 key.
func rateLimitIP(ip netip.Addr) netip.Addr {
	ip = ip.Unmap().WithZone("")
	if ip.Is6() {
		return netip.PrefixFrom(ip, 64).Masked().Addr()
	}
	return ip
}

func takeWindow(w ipWindow, now time.Time, burst int) (ipWindow, bool) {
	if w.start.IsZero() || !now.Before(w.start.Add(time.Minute)) {
		w = ipWindow{start: now}
	}
	if w.used >= burst {
		return w, false
	}
	w.used++
	return w, true
}
func (s *Service) admitIP(ip netip.Addr, now time.Time) bool {
	return s.admitIPClass(ip, now, false)
}
func (s *Service) admitIPClass(ip netip.Addr, now time.Time, candidate bool) bool {
	ip = rateLimitIP(ip)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Both arrival classes share an existing IP counter. Presenting or removing
	// a device cookie must never give the same IP an additional burst.
	if w, exists := s.ips[ip]; exists {
		next, ok := takeWindow(w, now, s.cfg.IPBurst)
		s.ips[ip] = next
		return ok
	}
	if w, exists := s.knownIPs[ip]; exists {
		next, ok := takeWindow(w, now, s.cfg.IPBurst)
		s.knownIPs[ip] = next
		return ok
	}
	if _, exists := s.ips[ip]; !exists && len(s.ips) >= 4096 {
		for key, w := range s.ips {
			if !now.Before(w.start.Add(time.Minute)) {
				delete(s.ips, key)
			}
		}
		if len(s.ips) >= 4096 {
			if !candidate {
				return false
			}
			if len(s.knownIPs) >= 256 {
				for key, w := range s.knownIPs {
					if !now.Before(w.start.Add(time.Minute)) {
						delete(s.knownIPs, key)
					}
				}
			}
			if len(s.knownIPs) >= 256 {
				return false
			}
			w, ok := takeWindow(ipWindow{}, now, s.cfg.IPBurst)
			s.knownIPs[ip] = w
			return ok
		}
	}
	w, ok := takeWindow(s.ips[ip], now, s.cfg.IPBurst)
	s.ips[ip] = w
	return ok
}
func (s *Service) admitGlobal(known bool, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	reserved := s.cfg.GlobalBurst / 10
	if reserved < 1 {
		reserved = 1
	}
	if known {
		w, ok := takeWindow(s.knownGlobal, now, reserved)
		s.knownGlobal = w
		return ok
	}
	w, ok := takeWindow(s.global, now, s.cfg.GlobalBurst-reserved)
	s.global = w
	return ok
}
