// Responsibilities: independent auth command invariants through the public port.
// Dependencies: fake atomic repository, runtime-generated credentials and stdlib;
// CI executes these tests. Invariants: no literal secrets, no raw DB connections,
// and fake transaction proof does not substitute for PostgreSQL race acceptance.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
)

const tenantA = "11111111-1111-4111-8111-111111111111"
const tenantB = "22222222-2222-4222-8222-222222222222"
const buyerID = "33333333-3333-4333-8333-333333333333"
const buyerEmail = "buyer@example.test"
const noPreviousToken = ""
const privateMarker = "private"
const normalizedInput = "  BUYER@example.test  "

type challenge struct {
	hash    string
	expires time.Time
	used    bool
}
type fakeRepo struct {
	mu                  sync.Mutex
	now                 time.Time
	credential          c.Credential
	registration, reset map[[32]byte]challenge
	sessions            map[[32]byte]c.Session
	preparedHash        string
	admissions          int
	failures            int
	accounts, ips       [][32]byte
	retry               time.Duration
	err                 error
}

func (r *fakeRepo) Credential(_ context.Context, tenant, email string) (c.Credential, error) {
	if r.err != nil {
		return c.Credential{}, r.err
	}
	if tenant != tenantA || email != buyerEmail || r.credential.CustomerID == "" {
		return c.Credential{}, c.ErrNotFound
	}
	return r.credential, nil
}
func (r *fakeRepo) PrepareRegistration(_ context.Context, tenant, email, hash string, token [32]byte, expires time.Time) error {
	if tenant != tenantA || email != buyerEmail {
		return c.ErrInvalidInput
	}
	r.preparedHash = hash
	r.registration[token] = challenge{hash: hash, expires: expires}
	return nil
}
func (r *fakeRepo) ConsumeVerification(_ context.Context, tenant string, token [32]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.registration[token]
	if tenant != tenantA || !ok || ch.used || !ch.expires.After(r.now) {
		return c.ErrToken
	}
	ch.used = true
	r.registration[token] = ch
	r.credential = c.Credential{CustomerID: buyerID, Email: buyerEmail, Hash: ch.hash, Verified: true, Active: true}
	return nil
}
func (r *fakeRepo) PrepareReset(_ context.Context, tenant, email string, token [32]byte, expires time.Time) error {
	if tenant != tenantA || email != buyerEmail || !r.credential.Verified || !r.credential.Active {
		return nil
	}
	r.reset[token] = challenge{expires: expires}
	return nil
}
func (r *fakeRepo) ConsumeReset(_ context.Context, tenant string, token [32]byte, hash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.reset[token]
	if tenant != tenantA || !ok || ch.used || !ch.expires.After(r.now) {
		return c.ErrToken
	}
	ch.used = true
	r.reset[token] = ch
	r.credential.Hash = hash
	r.sessions = make(map[[32]byte]c.Session)
	return nil
}
func (r *fakeRepo) SaveSession(_ context.Context, tenant, customer, expectedHash string, old *[32]byte, next [32]byte, absolute, idle time.Time) (c.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tenant != tenantA || customer != buyerID || expectedHash != r.credential.Hash {
		return c.Session{}, c.ErrUnauthorized
	}
	if old != nil {
		prior, ok := r.sessions[*old]
		if !ok || !prior.ExpiresAt.After(r.now) || !prior.IdleExpiresAt.After(r.now) {
			return c.Session{}, c.ErrUnauthorized
		}
		delete(r.sessions, *old)
	}
	s := c.Session{ID: buyerID, TenantID: tenant, CustomerID: customer, Email: buyerEmail, ExpiresAt: absolute, IdleExpiresAt: idle}
	r.sessions[next] = s
	return s, nil
}
func (r *fakeRepo) Session(_ context.Context, tenant string, hash [32]byte, idle time.Duration) (c.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[hash]
	if !ok || s.TenantID != tenant || !s.ExpiresAt.After(r.now) || !s.IdleExpiresAt.After(r.now) || !r.credential.Active || !r.credential.Verified {
		return c.Session{}, c.ErrUnauthorized
	}
	s.IdleExpiresAt = r.now.Add(idle)
	if s.ExpiresAt.Before(s.IdleExpiresAt) {
		s.IdleExpiresAt = s.ExpiresAt
	}
	r.sessions[hash] = s
	return s, nil
}
func (r *fakeRepo) RevokeSession(_ context.Context, tenant string, hash [32]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[hash]
	if !ok || s.TenantID != tenant {
		return c.ErrUnauthorized
	}
	delete(r.sessions, hash)
	return nil
}
func (r *fakeRepo) Admit(_ context.Context, tenant string, account, ip [32]byte, now time.Time) (time.Duration, error) {
	r.admissions++
	r.accounts = append(r.accounts, account)
	r.ips = append(r.ips, ip)
	return r.retry, r.err
}
func (r *fakeRepo) Failed(context.Context, string, [32]byte, [32]byte) error {
	r.failures++
	return r.err
}

type mail struct{ email, purpose, token string }
type fakeMailer struct {
	messages []mail
	err      error
}

func (m *fakeMailer) Send(_ context.Context, tenant, email, purpose, token string) error {
	if m.err != nil {
		return m.err
	}
	m.messages = append(m.messages, mail{email, purpose, token})
	return nil
}

func opaque(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		t.Fatal(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func hashToken(t *testing.T, token string) [32]byte {
	t.Helper()
	b, e := base64.RawURLEncoding.Strict().DecodeString(token)
	if e != nil || len(b) != 32 {
		t.Fatal("noncanonical token")
	}
	return sha256.Sum256(b)
}
func phcForTest(t *testing.T) string {
	t.Helper()
	salt := make([]byte, 16)
	if _, e := rand.Read(salt); e != nil {
		t.Fatal(e)
	}
	return "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))
}
func fixture(t *testing.T) (*Service, *fakeRepo, *fakeMailer, *int) {
	t.Helper()
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	r := &fakeRepo{now: now, registration: make(map[[32]byte]challenge), reset: make(map[[32]byte]challenge), sessions: make(map[[32]byte]c.Session)}
	m := &fakeMailer{}
	key := make([]byte, 32)
	if _, e := rand.Read(key); e != nil {
		t.Fatal(e)
	}
	s, e := New(Config{Repo: r, Mailer: m, Now: func() time.Time { return r.now }, Secret: key, HashSlots: 1, HashTimeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	calls := new(int)
	s.deriveKey = func(_, _ []byte, iterations, memory uint32, parallel uint8, length uint32) []byte {
		*calls++
		if iterations != 2 || memory != 19456 || parallel != 1 || length != 32 {
			t.Error("Argon2id parameters changed")
		}
		return make([]byte, 32)
	}
	return s, r, m, calls
}
func enabled(t *testing.T, r *fakeRepo) {
	t.Helper()
	r.credential = c.Credential{CustomerID: buyerID, Email: buyerEmail, Hash: phcForTest(t), Verified: true, Active: true}
}

var testIP = netip.MustParseAddr("192.0.2.1")

func TestCustomerAuthRegistrationRequiresOwnershipAndSingleUse(t *testing.T) {
	s, r, m, calls := fixture(t)
	password := opaque(t)
	if e := s.Register(context.Background(), tenantA, normalizedInput, password, testIP); e != nil {
		t.Fatalf("CUSTOMER_AUTH_REGISTRATION_RED: %v", e)
	}
	if r.credential.CustomerID != "" || *calls != 1 || len(m.messages) != 1 {
		t.Fatal("registration established identity before proof or omitted bounded password work")
	}
	phcPrefix := "$argon2id$v=19$m=19456,t=2,p=1$"
	if !strings.HasPrefix(r.preparedHash, phcPrefix) || strings.Contains(r.preparedHash, password) {
		t.Fatal("password persistence is not PHC")
	}
	msg := m.messages[0]
	if msg.purpose != "register" || msg.email != buyerEmail {
		t.Fatal("wrong verification delivery")
	}
	if _, ok := r.registration[hashToken(t, msg.token)]; !ok {
		t.Fatal("plaintext challenge or wrong hash persisted")
	}
	if e := s.Verify(context.Background(), tenantB, msg.token); !errors.Is(e, c.ErrToken) {
		t.Fatal("cross-tenant challenge admitted")
	}
	if e := s.Verify(context.Background(), tenantA, msg.token); e != nil {
		t.Fatal(e)
	}
	if e := s.Verify(context.Background(), tenantA, msg.token); !errors.Is(e, c.ErrToken) {
		t.Fatal("verification replay admitted")
	}
}
func TestCustomerAuthEqualWorkAndGenericCredentialResponse(t *testing.T) {
	for _, kind := range []string{"unknown", "disabled", "unverified", "wrong", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			s, r, _, calls := fixture(t)
			enabled(t, r)
			password := opaque(t)
			switch kind {
			case "unknown":
				r.credential = c.Credential{}
			case "disabled":
				r.credential.Active = false
			case "unverified":
				r.credential.Verified = false
			case "wrong":
				r.credential.Hash = strings.TrimSuffix(r.credential.Hash, strings.Repeat("A", 43)) + base64.RawStdEncoding.EncodeToString([]byte(strings.Repeat("b", 32)))
			case "malformed":
				r.credential.Hash = "$argon2id$v=19$m=999999999,t=999999,p=99$bad$bad"
			}
			_, e := s.Login(context.Background(), tenantA, buyerEmail, password, testIP, noPreviousToken)
			if !errors.Is(e, c.ErrInvalidCredentials) || e.Error() != c.ErrInvalidCredentials.Error() || *calls != 1 || r.failures != 1 || len(r.sessions) != 0 {
				t.Fatalf("CUSTOMER_AUTH_EQUAL_WORK_RED: err=%v hashes=%d failures=%d", e, *calls, r.failures)
			}
		})
	}
}
func TestCustomerAuthPasswordBoundsBeforeWork(t *testing.T) {
	s, r, _, calls := fixture(t)
	password := opaque(t)
	prefix := string(rune(0))
	tooLong := strings.Repeat("a", 1025)
	for _, input := range []string{password[:11], tooLong, prefix + password} {
		if e := s.Register(context.Background(), tenantA, buyerEmail, input, testIP); !errors.Is(e, c.ErrInvalidInput) {
			t.Errorf("invalid new password accepted: %v", e)
		}
	}
	if _, e := s.Login(context.Background(), tenantA, buyerEmail, strings.Repeat("a", 1025), testIP, ""); !errors.Is(e, c.ErrInvalidInput) {
		t.Errorf("oversized login accepted: %v", e)
	}
	if *calls != 0 || r.admissions != 0 {
		t.Fatal("invalid input reached hashing/admission")
	}
	if e := s.Register(context.Background(), tenantA, buyerEmail, strings.Repeat("a", 1024), testIP); e != nil {
		t.Fatalf("1024-byte password refused: %v", e)
	}
}
func TestCustomerAuthRateLimitImmediateAndIPv6Subnet(t *testing.T) {
	s, r, _, calls := fixture(t)
	r.retry = 37 * time.Second
	password := opaque(t)
	for _, ip := range []netip.Addr{netip.MustParseAddr("2001:db8:abcd:1234::1"), netip.MustParseAddr("2001:db8:abcd:1234::ffff")} {
		start := time.Now()
		_, e := s.Login(context.Background(), tenantA, buyerEmail, password, ip, noPreviousToken)
		var limited *c.RateLimitError
		if !errors.Is(e, c.ErrRateLimited) || !errors.As(e, &limited) || limited.RetryAfter != r.retry {
			t.Fatalf("CUSTOMER_AUTH_RATE_RED: %v", e)
		}
		if time.Since(start) > 250*time.Millisecond {
			t.Fatal("cooldown slept")
		}
	}
	if *calls != 0 || len(r.ips) != 2 || r.ips[0] != r.ips[1] {
		t.Fatal("rate denial performed KDF or IPv6 addresses evade /64 bucket")
	}
}
func TestCustomerAuthLoginRotationAndSessionIsolation(t *testing.T) {
	s, r, _, _ := fixture(t)
	enabled(t, r)
	password := opaque(t)
	first, e := s.Login(context.Background(), tenantA, buyerEmail, password, testIP, noPreviousToken)
	if e != nil {
		t.Fatalf("CUSTOMER_AUTH_LOGIN_RED: %v", e)
	}
	if first.Token == "" || first.TenantID != tenantA || first.CustomerID != buyerID || !first.ExpiresAt.After(r.now) || first.IdleExpiresAt.After(first.ExpiresAt) {
		t.Fatal("invalid session")
	}
	data, e := json.Marshal(first)
	if e != nil || strings.Contains(string(data), first.Token) {
		t.Fatal("session token leaked into JSON")
	}
	next, e := s.Login(context.Background(), tenantA, buyerEmail, password, testIP, first.Token)
	if e != nil {
		t.Fatal(e)
	}
	if first.Token == next.Token || len(r.sessions) != 1 {
		t.Fatal("rotation did not replace session")
	}
	if _, e = s.Current(context.Background(), tenantA, first.Token); !errors.Is(e, c.ErrUnauthorized) {
		t.Fatal("old session replay admitted")
	}
	if _, e = s.Current(context.Background(), tenantB, next.Token); !errors.Is(e, c.ErrUnauthorized) {
		t.Fatal("session crossed tenant")
	}
	if _, e = s.Login(context.Background(), tenantA, buyerEmail, password, testIP, first.Token); !errors.Is(e, c.ErrUnauthorized) {
		t.Fatal("second successor admitted")
	}
	if len(r.sessions) != 1 {
		t.Fatal("failed rotation changed current sessions")
	}
	if e = s.Logout(context.Background(), tenantA, next.Token); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Current(context.Background(), tenantA, next.Token); !errors.Is(e, c.ErrUnauthorized) {
		t.Fatal("revoked session admitted")
	}
}
func TestCustomerAuthExpiredVerificationAndResetRevokesSessions(t *testing.T) {
	s, r, m, _ := fixture(t)
	password := opaque(t)
	if e := s.Register(context.Background(), tenantA, buyerEmail, password, testIP); e != nil {
		t.Fatalf("CUSTOMER_AUTH_RESET_RED: %v", e)
	}
	verification := m.messages[0].token
	r.now = r.now.Add(25 * time.Hour)
	if e := s.Verify(context.Background(), tenantA, verification); !errors.Is(e, c.ErrToken) {
		t.Fatal("expired verification admitted")
	}
	enabled(t, r)
	session, e := s.Login(context.Background(), tenantA, buyerEmail, password, testIP, noPreviousToken)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.RequestReset(context.Background(), tenantA, buyerEmail, testIP); e != nil {
		t.Fatal(e)
	}
	msg := m.messages[len(m.messages)-1]
	if msg.purpose != "reset" {
		t.Fatal("wrong reset delivery")
	}
	if e = s.Reset(context.Background(), tenantB, msg.token, password, testIP); !errors.Is(e, c.ErrToken) {
		t.Fatal("reset crossed tenant")
	}
	newPassword := opaque(t)
	if e = s.Reset(context.Background(), tenantA, msg.token, newPassword, testIP); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Current(context.Background(), tenantA, session.Token); !errors.Is(e, c.ErrUnauthorized) {
		t.Fatal("reset retained session")
	}
	if e = s.Reset(context.Background(), tenantA, msg.token, newPassword, testIP); !errors.Is(e, c.ErrToken) {
		t.Fatal("reset replay admitted")
	}
	if e = s.RequestReset(context.Background(), tenantA, buyerEmail, testIP); e != nil {
		t.Fatal(e)
	}
	expired := m.messages[len(m.messages)-1].token
	r.now = r.now.Add(25 * time.Hour)
	if e = s.Reset(context.Background(), tenantA, expired, newPassword, testIP); !errors.Is(e, c.ErrToken) {
		t.Fatal("expired reset admitted")
	}
}
func TestCustomerAuthUnknownResetAndInvalidTokenAreOpaque(t *testing.T) {
	s, r, m, _ := fixture(t)
	if e := s.RequestReset(context.Background(), tenantA, buyerEmail, testIP); e != nil || len(m.messages) != 0 {
		t.Fatalf("unknown reset response reveals account: %v", e)
	}
	for _, token := range []string{"", strings.Repeat("A", 42), strings.Repeat("A", 44), "%" + strings.Repeat("A", 42)} {
		if e := s.Verify(context.Background(), tenantA, token); !errors.Is(e, c.ErrToken) {
			t.Errorf("malformed verification token: %v", e)
		}
		if _, e := s.Current(context.Background(), tenantA, token); !errors.Is(e, c.ErrUnauthorized) {
			t.Errorf("malformed session token: %v", e)
		}
	}
	if len(r.sessions) != 0 {
		t.Fatal("malformed input created session")
	}
}
func TestCustomerAuthRepositoryAndMailerFailuresAreGeneric(t *testing.T) {
	s, r, _, calls := fixture(t)
	password := opaque(t)
	r.err = errors.New("private database diagnostic")
	if _, e := s.Login(context.Background(), tenantA, buyerEmail, password, testIP, noPreviousToken); !errors.Is(e, c.ErrUnavailable) || strings.Contains(e.Error(), privateMarker) {
		t.Fatal("database error leaked")
	}
	if *calls != 0 {
		t.Fatal("unavailable repository hashed")
	}
	s, _, m, _ := fixture(t)
	m.err = errors.New("private delivery diagnostic")
	if e := s.Register(context.Background(), tenantA, buyerEmail, password, testIP); !errors.Is(e, c.ErrUnavailable) || strings.Contains(e.Error(), privateMarker) {
		t.Fatal("delivery error leaked")
	}
}

func TestCustomerAuthKDFDeadlineRetainsCapacityUntilCompletion(t *testing.T) {
	s, r, _, _ := fixture(t)
	enabled(t, r)
	s.cfg.HashTimeout = 10 * time.Millisecond
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	defer close(release)
	s.deriveKey = func(_, _ []byte, _, _ uint32, _ uint8, _ uint32) []byte {
		entered <- struct{}{}
		<-release
		return make([]byte, 32)
	}
	password := opaque(t)
	finished := make(chan error, 1)
	go func() {
		_, e := s.Login(context.Background(), tenantA, buyerEmail, password, testIP, noPreviousToken)
		finished <- e
	}()
	select {
	case <-entered:
	case <-finished:
		t.Fatal("CUSTOMER_AUTH_KDF_TIMEOUT_RED: no KDF was admitted")
	case <-time.After(time.Second):
		t.Fatal("KDF admission blocked")
	}
	select {
	case e := <-finished:
		if !errors.Is(e, c.ErrRateLimited) {
			t.Fatalf("deadline must return429: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("unbounded KDF deadline")
	}
	_, e := s.Login(context.Background(), tenantA, buyerEmail, password, testIP, noPreviousToken)
	if !errors.Is(e, c.ErrRateLimited) {
		t.Fatalf("timed-out worker released occupied slot: %v", e)
	}
	select {
	case <-entered:
		t.Fatal("new background KDF escaped concurrency bound")
	default:
	}
	if len(r.sessions) != 0 || r.failures != 0 {
		t.Fatal("deadline performed late repository mutation")
	}
}

func TestCustomerAuthConfigurationAndSessionExpiryAreFailClosed(t *testing.T) {
	if _, e := New(Config{}); !errors.Is(e, c.ErrInvalidInput) {
		t.Fatalf("CUSTOMER_AUTH_CONFIG_RED: invalid configuration accepted: %v", e)
	}
	s, r, _, _ := fixture(t)
	enabled(t, r)
	password := opaque(t)
	session, e := s.Login(context.Background(), tenantA, buyerEmail, password, testIP, noPreviousToken)
	if e != nil {
		t.Fatal(e)
	}
	r.now = session.IdleExpiresAt
	if _, e = s.Current(context.Background(), tenantA, session.Token); !errors.Is(e, c.ErrUnauthorized) {
		t.Fatal("idle-expired session admitted")
	}
}

func TestCustomerAuthResetDuringKDFCannotCreateStaleSession(t *testing.T) {
	s,r,_,_:=fixture(t)
	enabled(t,r)
	replacement:=phcForTest(t)
	s.deriveKey=func(_,_ []byte,_,_ uint32,_ uint8,_ uint32) []byte {
		r.credential.Hash=replacement
		r.sessions=make(map[[32]byte]c.Session)
		return make([]byte,32)
	}
	password:=opaque(t)
	_,e:=s.Login(context.Background(),tenantA,buyerEmail,password,testIP,noPreviousToken)
	if !errors.Is(e,c.ErrUnauthorized) || len(r.sessions)!=0 {
		t.Fatalf("CUSTOMER_AUTH_STALE_KDF_RED: reset allowed stale session admission: %v",e)
	}
}
