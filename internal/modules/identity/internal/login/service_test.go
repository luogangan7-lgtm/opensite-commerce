// Tests own login admission, cryptographic binding and reserved capacity gates.
// Dependencies: fake repository and Argon2; callers are Go's focused test runner.
// Invariant: this suite proves service behavior, not real PostgreSQL transactions.
package login

import (
	"context"
	"encoding/base64"
	"errors"
	"golang.org/x/crypto/argon2"
	"net/netip"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeRepo struct {
	credentialHook                                 func(context.Context, string) (Credential, error)
	cooldownIP, failureIP                          netip.Addr
	credential                                     Credential
	credentialErr                                  error
	credentialCalls                                int
	device                                         Device
	session                                        Session
	account, accountIP                             time.Time
	cooldownCalls, failures, deviceFailures, saves int
	saved                                          SessionInput
	failureErr, deviceFailureErr                   error
	lookupHook                                     func() (Session, error)
	cooldownHook                                   func(string, netip.Addr) (time.Time, time.Time, error)
}

func (f *fakeRepo) Credential(ctx context.Context, identifier string) (Credential, error) {
	f.credentialCalls++
	if f.credentialHook != nil {
		return f.credentialHook(ctx, identifier)
	}
	return f.credential, f.credentialErr
}

func TestGlobalAdmissionPrecedesDatabase(t *testing.T) {
	now := time.Unix(10000, 0)
	r := &fakeRepo{credential: activeCredential()}
	cfg := testConfig(&now)
	cfg.GlobalBurst = 2
	s, _ := New(r, cfg)
	s.admitGlobal(false, now)
	_, e := s.Login(context.Background(), input())
	if !errors.Is(e, ErrRateLimited) || r.credentialCalls != 0 {
		t.Fatalf("global rejected after DB: err=%v calls=%d", e, r.credentialCalls)
	}
}

func TestKnownNewIPWhenOrdinaryMapFull(t *testing.T) {
	now := time.Unix(10000, 0)
	r := &fakeRepo{credential: activeCredential(), device: Device{ID: "dev", UserID: "user", ExpiresAt: now.Add(time.Hour)}}
	cfg := testConfig(&now)
	cfg.IPBurst = 1
	s, _ := New(r, cfg)
	for i := 0; i < 4096; i++ {
		s.admitIP(netip.AddrFrom4([4]byte{192, 0, byte(i >> 8), byte(i)}), now)
	}
	raw, _ := newToken()
	in := input()
	in.IP = netip.MustParseAddr("198.51.100.1")
	in.DeviceToken = s.sign("device", raw)
	if _, e := s.Login(context.Background(), in); e != nil {
		t.Fatalf("owner starved by map capacity %v", e)
	}
	if _, e := s.Login(context.Background(), in); !errors.Is(e, ErrRateLimited) {
		t.Fatalf("known bypassed IP burst %v", e)
	}
}

// Two +60s requests for one account previously held both default ordinary slots.
// Channel rendezvous make the unrelated-owner observation deterministic without sleeps.
func TestCooldownDoesNotStarveUnrelatedOwner(t *testing.T) {
	now := time.Unix(10000, 0)
	r := &fakeRepo{credential: activeCredential()}
	r.cooldownHook = func(identifier string, _ netip.Addr) (time.Time, time.Time, error) {
		if identifier == "staff@example.com" {
			return now.Add(60 * time.Second), time.Time{}, nil
		}
		return time.Time{}, time.Time{}, nil
	}
	cfg := testConfig(&now)
	entered := make(chan time.Duration, 2)
	release := make(chan struct{})
	done := make(chan error, 2)
	cfg.Wait = func(_ context.Context, d time.Duration) error { entered <- d; <-release; return context.Canceled }
	s, _ := New(r, cfg)
	completed := 0
	for i := 0; i < 2; i++ {
		go func() { _, e := s.Login(context.Background(), input()); done <- e }()
		select {
		case d := <-entered:
			if d != 60*time.Second {
				t.Fatalf("cooldown %v", d)
			}
		case e := <-done:
			completed++
			if !errors.Is(e, ErrRateLimited) {
				t.Fatalf("cooldown returned %v", e)
			}
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("cooldown failed to respond")
		}
	}
	owner := input()
	owner.Identifier = "owner@example.com"
	owner.IP = netip.MustParseAddr("198.51.100.2")
	_, ownerErr := s.Login(context.Background(), owner)
	held := len(s.admissionSlots)
	close(release)
	for completed < 2 {
		select {
		case e := <-done:
			completed++
			if !errors.Is(e, ErrRateLimited) {
				t.Fatalf("cooldown returned %v", e)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("cooldown did not release")
		}
	}
	if ownerErr != nil {
		t.Fatalf("unrelated owner rejected: %v; cooldown held %d ordinary admission slots", ownerErr, held)
	}
	if held != 0 || r.failures != 0 || r.saves != 1 {
		t.Fatalf("cooldown consumed resources: held=%d failures=%d saves=%d", held, r.failures, r.saves)
	}
}

func TestDeviceCandidateCannotKeepReservedAfterValidation(t *testing.T) {
	for _, forged := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong-user", true: "forged"}[forged], func(t *testing.T) {
			now := time.Unix(10000, 0)
			r := &fakeRepo{credential: activeCredential(), device: Device{ID: "dev", UserID: "other", ExpiresAt: now.Add(time.Hour)}}
			cfg := testConfig(&now)
			cfg.GlobalBurst = 2
			s, _ := New(r, cfg)
			s.admitGlobal(false, now)
			raw, _ := newToken()
			in := input()
			in.DeviceToken = s.sign("device", raw)
			if forged {
				in.DeviceToken += "x"
			}
			_, e := s.Login(context.Background(), in)
			if !errors.Is(e, ErrRateLimited) || r.cooldownCalls != 0 || len(s.admissionSlots) != 0 || len(s.candidateSlots) != 0 {
				t.Fatalf("invalid device retained reserved admission: %v", e)
			}
			if forged && r.credentialCalls != 0 {
				t.Fatal("forged token used reserved DB admission")
			}
		})
	}
}

func TestReservedIPMapBoundedAndCounterShared(t *testing.T) {
	now := time.Unix(10000, 0)
	cfg := testConfig(&now)
	cfg.IPBurst = 1
	s, _ := New(&fakeRepo{}, cfg)
	for i := 0; i < 4096; i++ {
		s.admitIP(netip.AddrFrom4([4]byte{192, 0, byte(i >> 8), byte(i)}), now)
	}
	for i := 0; i < 256; i++ {
		ip := netip.AddrFrom4([4]byte{198, 51, 100, byte(i)})
		if !s.admitIPClass(ip, now, true) {
			t.Fatal("reserved map full early")
		}
		if s.admitIP(ip, now) {
			t.Fatal("cookie removal granted second burst")
		}
	}
	if s.admitIPClass(netip.MustParseAddr("203.0.113.1"), now, true) || len(s.knownIPs) != 256 {
		t.Fatal("reserved map unbounded")
	}
	now = now.Add(time.Minute)
	if !s.admitIPClass(netip.MustParseAddr("203.0.113.1"), now, true) {
		t.Fatal("expired capacity not reclaimed")
	}
}
func (f *fakeRepo) Cooldowns(_ context.Context, identifier string, ip netip.Addr) (time.Time, time.Time, error) {
	f.cooldownCalls++
	f.cooldownIP = ip
	if f.cooldownHook != nil {
		return f.cooldownHook(identifier, ip)
	}
	return f.account, f.accountIP, nil
}
func (f *fakeRepo) RecordFailure(_ context.Context, _ string, ip netip.Addr) error {
	f.failures++
	f.failureIP = ip
	return f.failureErr
}
func (f *fakeRepo) Device(context.Context, [32]byte) (Device, error) {
	if f.device.ID == "" || !f.device.ExpiresAt.After(time.Unix(10000, 0)) {
		return Device{}, ErrNotFound
	}
	return f.device, nil
}
func (f *fakeRepo) FailDevice(context.Context, string) error {
	f.deviceFailures++
	return f.deviceFailureErr
}
func (f *fakeRepo) SaveLogin(_ context.Context, i SessionInput) (Session, error) {
	f.saves++
	f.saved = i
	return Session{DatabaseNow: i.CreatedAt, ID: "session", UserID: i.UserID, CreatedAt: i.CreatedAt, LastSeenAt: i.CreatedAt, ExpiresAt: i.ExpiresAt}, nil
}
func (f *fakeRepo) LookupSession(context.Context, [32]byte, time.Time, time.Duration) (Session, error) {
	if f.lookupHook != nil {
		return f.lookupHook()
	}
	if f.session.ID == "" {
		return Session{}, ErrNotFound
	}
	return f.session, nil
}
func (f *fakeRepo) RevokeSession(context.Context, string, time.Time) error { return nil }

func TestAuthenticateUsesDatabaseCompletionClockAfterReorderedLookup(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		created, last, expires time.Duration
		wantError              bool
	}{{"monotonic touch while older request waited", -time.Minute, 5 * time.Second, time.Hour, false}, {"future activity still denied", -time.Minute, 20 * time.Second, time.Hour, true}, {"future created still denied", 20 * time.Second, 5 * time.Second, time.Hour, true}, {"expires while waiting", -time.Minute, 5 * time.Second, 9 * time.Second, true}} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Unix(10000, 0)
			now := start
			r := &fakeRepo{}
			r.lookupHook = func() (Session, error) {
				now = start.Add(10 * time.Second)
				return Session{DatabaseNow: start.Add(10 * time.Second), ID: "session", UserID: "user", CreatedAt: start.Add(tc.created), LastSeenAt: start.Add(tc.last), ExpiresAt: start.Add(tc.expires)}, nil
			}
			s, _ := New(r, testConfig(&now))
			raw, _ := newToken()
			a, _, e := s.Authenticate(context.Background(), raw)
			if tc.wantError {
				if !errors.Is(e, ErrSession) {
					t.Fatalf("invalid timestamp admitted %v", e)
				}
			} else if e != nil || a.UserID() != "user" {
				t.Fatalf("monotonic session denied by pre-lookup clock %v", e)
			}
		})
	}
}
func testConfig(now *time.Time) Config {
	return Config{Origin: "https://admin.example.com", SigningKey: []byte(strings.Repeat("k", 32)), Now: func() time.Time { return *now }}
}
func activeCredential() Credential {
	salt := []byte("0123456789abcdef")
	hash := argon2.IDKey([]byte("correct-password"), salt, 2, 19456, 1, 32)
	return Credential{UserID: "user", Status: "active", PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash)}
}
func input() LoginInput {
	return LoginInput{Identifier: "staff@example.com", Password: "correct-password", IP: netip.MustParseAddr("192.0.2.1")}
}

func TestFailureWritesBothAttemptedAndRedacted(t *testing.T) {
	for _, which := range []string{"attempt", "device"} {
		t.Run(which, func(t *testing.T) {
			now := time.Unix(10000, 0)
			r := &fakeRepo{credential: activeCredential(), device: Device{ID: "dev", UserID: "user", ExpiresAt: now.Add(time.Hour)}}
			if which == "attempt" {
				r.failureErr = errors.New("SQL secret")
			} else {
				r.deviceFailureErr = errors.New("SQL secret")
			}
			s, _ := New(r, testConfig(&now))
			raw, _ := newToken()
			in := input()
			in.DeviceToken = s.sign("device", raw)
			in.Password = "wrong"
			_, e := s.Login(context.Background(), in)
			if !errors.Is(e, ErrDatabase) || r.failures != 1 || r.deviceFailures != 1 || strings.Contains(e.Error(), "secret") {
				t.Fatalf("bookkeeping not both attempted err=%v attempts=%d device=%d", e, r.failures, r.deviceFailures)
			}
		})
	}
}

func TestCryptoBindings(t *testing.T) {
	now := time.Unix(10000, 0)
	s, e := New(&fakeRepo{}, testConfig(&now))
	if e != nil {
		t.Fatal(e)
	}
	token, e := newToken()
	if e != nil {
		t.Fatal(e)
	}
	csrf := s.SessionCSRF(token)
	other, _ := newToken()
	if !s.VerifySessionCSRF(token, csrf) || s.VerifySessionCSRF(other, csrf) || s.VerifySessionCSRF(token+"x", csrf) || s.VerifySessionCSRF(token, csrf+"x") {
		t.Fatal("session CSRF not bound")
	}
	challenge, e := s.NewChallenge()
	if e != nil || !s.VerifyChallenge(challenge) {
		t.Fatal("challenge rejected")
	}
	now = now.Add(5 * time.Minute)
	if s.VerifyChallenge(challenge) {
		t.Fatal("expired challenge accepted")
	}
	for _, bad := range []string{"", token + "=", token + "\n", strings.Repeat("A", 42), strings.Repeat("A", 44)} {
		if _, e := tokenHash(bad); e == nil {
			t.Fatal("invalid token accepted")
		}
	}
}

func TestLoginFailureBookkeeping(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cred     Credential
		err      error
		password string
	}{{"unknown", Credential{}, ErrNotFound, "wrong"}, {"wrong", activeCredential(), nil, "wrong"}, {"disabled", Credential{UserID: "user", Status: "disabled", PasswordHash: activeCredential().PasswordHash}, nil, "correct-password"}, {"malicious PHC", Credential{UserID: "user", Status: "active", PasswordHash: "$argon2id$v=19$m=4294967295,t=2,p=1$abc$abc"}, nil, "correct-password"}} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(10000, 0)
			r := &fakeRepo{credential: tc.cred, credentialErr: tc.err}
			s, _ := New(r, testConfig(&now))
			calls := 0
			s.deriveKey = func(password, salt []byte, iterations, memory uint32, parallelism uint8, keyLength uint32) []byte {
				calls++
				if iterations != 2 || memory != 19456 || parallelism != 1 || keyLength != 32 || len(salt) != 16 {
					t.Fatal("unsafe Argon2 arguments")
				}
				return argon2.IDKey(password, salt, iterations, memory, parallelism, keyLength)
			}
			in := input()
			in.Password = tc.password
			_, e := s.Login(context.Background(), in)
			if !errors.Is(e, ErrCredentials) || r.failures != 1 || r.saves != 0 || calls != 1 {
				t.Fatalf("err=%v failures=%d saves=%d", e, r.failures, r.saves)
			}
		})
	}
}

func TestDeviceAdmissionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name             string
		user             string
		revoked, expired bool
		forged           bool
	}{{"mismatch", "another", false, false, false}, {"revoked", "user", true, false, false}, {"expired", "user", false, true, false}, {"forged", "user", false, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(10000, 0)
			r := &fakeRepo{credential: activeCredential(), account: now.Add(time.Minute), device: Device{ID: "dev", UserID: tc.user, Revoked: tc.revoked, ExpiresAt: now.Add(time.Hour)}}
			if tc.expired {
				r.device.ExpiresAt = now
			}
			cfg := testConfig(&now)
			s, _ := New(r, cfg)
			raw, _ := newToken()
			in := input()
			in.DeviceToken = s.sign("device", raw)
			if tc.forged {
				in.DeviceToken += "x"
			}
			if _, e := s.Login(context.Background(), in); !errors.Is(e, ErrRateLimited) || r.saves != 0 {
				t.Fatalf("invalid device exempted: %v saves=%d", e, r.saves)
			}
		})
	}
}

func TestKnownStillSubjectToIPAndAccountIP(t *testing.T) {
	for _, layer := range []string{"IP", "account-IP"} {
		t.Run(layer, func(t *testing.T) {
			now := time.Unix(10000, 0)
			r := &fakeRepo{credential: activeCredential(), device: Device{ID: "dev", UserID: "user", ExpiresAt: now.Add(time.Hour)}}
			cfg := testConfig(&now)
			cfg.IPBurst = 1
			s, _ := New(r, cfg)
			raw, _ := newToken()
			in := input()
			in.DeviceToken = s.sign("device", raw)
			if layer == "IP" {
				s.admitIP(in.IP, now)
			} else {
				r.accountIP = now.Add(time.Minute)
			}
			_, e := s.Login(context.Background(), in)
			if !errors.Is(e, ErrRateLimited) || r.failures != 0 || r.deviceFailures != 0 {
				t.Fatalf("known bypassed %s: %v", layer, e)
			}
		})
	}
}

func TestGlobalReservationIPMapAndTTL(t *testing.T) {
	now := time.Unix(10000, 0)
	cfg := testConfig(&now)
	cfg.GlobalBurst = 2
	s, _ := New(&fakeRepo{}, cfg)
	if !s.admitGlobal(false, now) || s.admitGlobal(false, now) || !s.admitGlobal(true, now) || s.admitGlobal(true, now) {
		t.Fatal("global reserved capacity wrong")
	}
	for i := 0; i < 4096; i++ {
		ip := netip.AddrFrom4([4]byte{192, 0, byte(i >> 8), byte(i)})
		if !s.admitIP(ip, now) {
			t.Fatal("map filled prematurely")
		}
	}
	newIP := netip.MustParseAddr("198.51.100.1")
	if s.admitIP(newIP, now) || len(s.ips) != 4096 {
		t.Fatal("unbounded IP map")
	}
	now = now.Add(time.Minute)
	if !s.admitIP(newIP, now) || len(s.ips) != 1 {
		t.Fatal("IP TTL not reclaimed")
	}
}

func TestSessionLifecycleAndRotationInput(t *testing.T) {
	now := time.Unix(10000, 0)
	r := &fakeRepo{credential: activeCredential()}
	s, _ := New(r, testConfig(&now))
	old, _ := newToken()
	in := input()
	in.OldSessionToken = old
	out, e := s.Login(context.Background(), in)
	if e != nil {
		t.Fatal(e)
	}
	h, _ := tokenHash(old)
	if r.saved.OldTokenHash == nil || *r.saved.OldTokenHash != h || out.SessionToken == old || r.saved.TokenHash == h {
		t.Fatal("rotation reused or omitted old token")
	}
	r.session = out.Session
	a, _, e := s.Authenticate(context.Background(), out.SessionToken)
	if e != nil || a.UserID() != "user" || a.SessionID() != "session" {
		t.Fatal("actor missing", e)
	}
	if e := s.Logout(context.Background(), a); e != nil {
		t.Fatal(e)
	}
	if _, ok := ActorFromContext(WithActor(context.Background(), a)); !ok {
		t.Fatal("context lost actor")
	}
	if _, ok := ActorFromContext(WithActor(context.Background(), Actor{})); ok {
		t.Fatal("zero actor admitted")
	}
	r.session.LastSeenAt = now.Add(-30 * time.Minute)
	if _, _, e := s.Authenticate(context.Background(), out.SessionToken); !errors.Is(e, ErrSession) {
		t.Fatal("idle expired admitted")
	}
	in.OldSessionToken = old + "="
	if _, e := s.Login(context.Background(), in); !errors.Is(e, ErrSession) || r.saves != 1 {
		t.Fatal("invalid old token admitted", e)
	}
}

func TestConfigOriginAndKeyCopy(t *testing.T) {
	now := time.Unix(10000, 0)
	for _, origin := range []string{"http://admin.example.com", "https://admin.example.com/", "https://user@admin.example.com", "https://admin.example.com?", "https://:443", "https://admin.example.com:"} {
		cfg := testConfig(&now)
		cfg.Origin = origin
		if _, e := New(&fakeRepo{}, cfg); e == nil {
			t.Fatalf("accepted origin %s", origin)
		}
	}
	cfg := testConfig(&now)
	s, e := New(&fakeRepo{}, cfg)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := newToken()
	csrf := s.SessionCSRF(raw)
	cfg.SigningKey[0] = 'x'
	if !s.VerifySessionCSRF(raw, csrf) {
		t.Fatal("caller mutated signing key")
	}
	for _, catchAll := range []string{"0.0.0.0/0", "::/0"} {
		cfg = testConfig(&now)
		cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix(catchAll)}
		if _, e := New(&fakeRepo{}, cfg); e == nil {
			t.Fatalf("accepted catch-all trusted proxy %s", catchAll)
		}
	}
	cfg = testConfig(&now)
	cfg.HashSlots = 16
	if _, e := New(&fakeRepo{}, cfg); e == nil {
		t.Fatal("memory bound missing")
	}
}

func TestCooldownImmediateAndKnownExemption(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "known"}[known], func(t *testing.T) {
			now := time.Unix(10000, 0)
			r := &fakeRepo{credential: activeCredential(), account: now.Add(10 * time.Hour)}
			cfg := testConfig(&now)
			cfg.Wait = func(context.Context, time.Duration) error { t.Fatal("cooldown slept"); return nil }
			s, _ := New(r, cfg)
			in := input()
			if known {
				raw, _ := newToken()
				r.device = Device{ID: "dev", UserID: "user", ExpiresAt: now.Add(time.Hour)}
				in.DeviceToken = s.sign("device", raw)
			}
			out, e := s.Login(context.Background(), in)
			if known {
				if e != nil || out.SessionToken == "" {
					t.Fatalf("known cooldown exemption lost: %v", e)
				}
			} else if !errors.Is(e, ErrRateLimited) || r.saves != 0 || r.failures != 0 {
				t.Fatalf("cooldown not rejected: %v", e)
			}
			if r.cooldownCalls != 1 || len(s.admissionSlots) != 0 || len(s.candidateSlots) != 0 {
				t.Fatal("cooldown leaked admission")
			}
		})
	}
}

func TestKnownDeviceCapacityAndFailure(t *testing.T) {
	now := time.Unix(10000, 0)
	r := &fakeRepo{credential: activeCredential(), device: Device{ID: "dev", UserID: "user", ExpiresAt: now.Add(time.Hour)}}
	s, _ := New(r, testConfig(&now))
	for i := 0; i < cap(s.hashSlots); i++ {
		s.hashSlots <- struct{}{}
	}
	raw, _ := newToken()
	in := input()
	in.DeviceToken = s.sign("device", raw)
	if _, e := s.Login(context.Background(), in); e != nil {
		t.Fatalf("known slot starved %v", e)
	}
	in.Password = "wrong"
	if _, e := s.Login(context.Background(), in); !errors.Is(e, ErrCredentials) || r.deviceFailures != 1 {
		t.Fatalf("device failure missing %v", e)
	}
	in.DeviceToken = ""
	if _, e := s.Login(context.Background(), in); !errors.Is(e, ErrRateLimited) || r.failures != 1 {
		t.Fatalf("rejected request recorded %v", e)
	}
}

// Cancellation bounds the client response but cannot cancel Argon2 itself.
// Both success and failure results arriving late must have zero durable writes.
func TestHashDeadlineRetainsCapacityAndNeverWritesLate(t *testing.T) {
	for _, correct := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong", true: "correct"}[correct], func(t *testing.T) {
			now := time.Unix(10000, 0)
			credential := activeCredential()
			r := &fakeRepo{credential: credential}
			var requestContext context.Context
			r.credentialHook = func(ctx context.Context, _ string) (Credential, error) { requestContext = ctx; return credential, nil }
			cfg := testConfig(&now)
			cfg.HashSlots = 1
			cfg.HashTimeout = 100 * time.Millisecond
			s, _ := New(r, cfg)
			entered := make(chan struct{})
			release := make(chan struct{})
			derived := make(chan struct{})
			var calls atomic.Int32
			s.deriveKey = func(password, salt []byte, times, memory uint32, threads uint8, size uint32) []byte {
				calls.Add(1)
				close(entered)
				<-release
				key := argon2.IDKey(password, salt, times, memory, threads, size)
				close(derived)
				return key
			}
			done := make(chan error, 1)
			in := input()
			if !correct {
				in.Password = "wrong"
			}
			go func() { _, e := s.Login(context.Background(), in); done <- e }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("KDF did not begin")
			}
			select {
			case <-requestContext.Done():
			case <-time.After(time.Second):
				close(release)
				t.Fatal("configured deadline missing")
			}
			select {
			case e := <-done:
				if !errors.Is(e, ErrRateLimited) {
					t.Fatalf("deadline response %v", e)
				}
			case <-time.After(time.Second):
				close(release)
				t.Fatal("blocked KDF blocked client response")
			}
			if len(s.hashSlots) != 1 || len(s.admissionSlots) != 0 || r.saves != 0 || r.failures != 0 {
				close(release)
				t.Fatal("deadline released KDF or retained DB admission/wrote")
			}
			if _, e := s.Login(context.Background(), input()); !errors.Is(e, ErrRateLimited) || calls.Load() != 1 {
				close(release)
				t.Fatalf("deadline allowed another KDF: %v calls=%d", e, calls.Load())
			}
			close(release)
			select {
			case <-derived:
			case <-time.After(3 * time.Second):
				t.Fatal("KDF did not finish")
			}
			deadline := time.Now().Add(time.Second)
			for len(s.hashSlots) != 0 {
				if time.Now().After(deadline) {
					t.Fatal("completed KDF leaked capacity")
				}
				runtime.Gosched()
			}
			if r.saves != 0 || r.failures != 0 || r.deviceFailures != 0 {
				t.Fatal("late KDF wrote durable state")
			}
		})
	}
}

func TestIPv6NetworkKeySharedAcrossAdmissionAndRepository(t *testing.T) {
	now := time.Unix(10000, 0)
	r := &fakeRepo{credential: activeCredential()}
	cfg := testConfig(&now)
	cfg.IPBurst = 1
	s, _ := New(r, cfg)
	in := input()
	in.Password = "wrong"
	in.IP = netip.MustParseAddr("2001:db8:1234:5678::1")
	if _, e := s.Login(context.Background(), in); !errors.Is(e, ErrCredentials) {
		t.Fatal(e)
	}
	want := netip.MustParseAddr("2001:db8:1234:5678::")
	if r.cooldownIP != want || r.failureIP != want {
		t.Fatalf("durable key cooldown=%v failure=%v", r.cooldownIP, r.failureIP)
	}
	in.IP = netip.MustParseAddr("2001:db8:1234:5678:ffff::2")
	if _, e := s.Login(context.Background(), in); !errors.Is(e, ErrRateLimited) || r.credentialCalls != 1 {
		t.Fatalf("same /64 bypassed admission %v reads=%d", e, r.credentialCalls)
	}
	in.IP = netip.MustParseAddr("2001:db8:1234:5679::1")
	if _, e := s.Login(context.Background(), in); !errors.Is(e, ErrCredentials) || r.credentialCalls != 2 {
		t.Fatalf("different /64 denied %v", e)
	}
	for _, raw := range []string{"192.0.2.1", "::ffff:192.0.2.1"} {
		if got := rateLimitIP(netip.MustParseAddr(raw)); got != netip.MustParseAddr("192.0.2.1") {
			t.Fatalf("IPv4 key %v", got)
		}
	}
}

func TestPasswordByteCapPrecedesDatabaseAndKDF(t *testing.T) {
	for _, length := range []int{1024, 1025} {
		t.Run(map[int]string{1024: "boundary", 1025: "over-cap"}[length], func(t *testing.T) {
			now := time.Unix(10000, 0)
			r := &fakeRepo{credential: activeCredential()}
			s, _ := New(r, testConfig(&now))
			var hashes atomic.Int32
			s.deriveKey = func([]byte, []byte, uint32, uint32, uint8, uint32) []byte { hashes.Add(1); return make([]byte, 32) }
			in := input()
			in.Password = strings.Repeat("p", length)
			if _, e := s.Login(context.Background(), in); !errors.Is(e, ErrCredentials) {
				t.Fatal(e)
			}
			want := map[int]int{1024: 1, 1025: 0}[length]
			if r.credentialCalls != want || int(hashes.Load()) != want || r.failures != want {
				t.Fatalf("length=%d reads=%d KDF=%d writes=%d", length, r.credentialCalls, hashes.Load(), r.failures)
			}
		})
	}
}

func TestDatabaseAdmissionHonorsLoginDeadline(t *testing.T) {
	now := time.Unix(10000, 0)
	r := &fakeRepo{}
	cfg := testConfig(&now)
	cfg.HashTimeout = 10 * time.Millisecond
	r.credentialHook = func(ctx context.Context, _ string) (Credential, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("repository context lacks deadline")
		}
		<-ctx.Done()
		return Credential{}, ctx.Err()
	}
	s, _ := New(r, cfg)
	s.deriveKey = func([]byte, []byte, uint32, uint32, uint8, uint32) []byte {
		t.Error("expired DB started KDF")
		return nil
	}
	if _, e := s.Login(context.Background(), input()); !errors.Is(e, ErrRateLimited) || len(s.admissionSlots) != 0 || r.cooldownCalls != 0 || r.failures != 0 || r.saves != 0 {
		t.Fatalf("DB deadline leaked or progressed: %v", e)
	}
}

// P1-2 (review of #43): MAC-valid but revoked, expired or wrong-user device
// cookies must not drain the reserved known-device budget.
func TestInvalidDeviceCookiesCannotDrainKnownReserve(t *testing.T) {
	for _, tc := range []struct {
		name   string
		device func(time.Time) Device
	}{
		{"revoked", func(now time.Time) Device {
			return Device{ID: "dev", UserID: "user", ExpiresAt: now.Add(time.Hour), Revoked: true}
		}},
		{"expired", func(now time.Time) Device { return Device{ID: "dev", UserID: "user", ExpiresAt: now.Add(-time.Second)} }},
		{"wrong-user", func(now time.Time) Device { return Device{ID: "dev", UserID: "other", ExpiresAt: now.Add(time.Hour)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(10000, 0)
			r := &fakeRepo{credential: activeCredential(), device: tc.device(now)}
			s, _ := New(r, testConfig(&now))
			raw, _ := newToken()
			cookie := s.sign("device", raw)
			for i := 0; i < 40; i++ {
				in := input()
				in.Password = strings.ToUpper(in.Password) // derived mismatch
				in.DeviceToken = cookie
				in.IP = netip.AddrFrom4([4]byte{198, 51, 100, byte(i)})
				if _, e := s.Login(context.Background(), in); !errors.Is(e, ErrCredentials) {
					t.Fatalf("attempt %d: %v", i, e)
				}
			}
			if s.knownGlobal.used != 0 {
				t.Fatalf("invalid device cookies consumed %d reserved admissions", s.knownGlobal.used)
			}
			r.device = Device{ID: "dev", UserID: "user", ExpiresAt: now.Add(time.Hour)}
			s.global = ipWindow{start: now, used: s.cfg.GlobalBurst} // ordinary bucket drained too
			in := input()
			in.DeviceToken = cookie
			if _, e := s.Login(context.Background(), in); e != nil {
				t.Fatalf("legitimate known device locked out: %v", e)
			}
		})
	}
}
