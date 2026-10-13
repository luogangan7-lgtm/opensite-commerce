// H-4 regressions exercise admission and response ordering through the real router.
// Dependencies: login service, in-memory repository and httptest; run only in CI.
// Database isolation/clock/credential fences have separate PostgreSQL witnesses.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"golang.org/x/crypto/argon2"
)

const h4Email = "owner@example.test"

type h4Repository struct {
	fakeRepo
	lookupCalls atomic.Int32
	saveCalls   atomic.Int32
	entered     chan struct{}
	release     chan struct{}
	rotate      bool
}

func (f *h4Repository) Credential(context.Context, string) (login.Credential, error) {
	return f.credential, nil
}
func (f *h4Repository) Cooldowns(context.Context, string, netip.Addr) (time.Time, time.Time, error) {
	return time.Time{}, time.Time{}, nil
}
func (f *h4Repository) LookupSession(ctx context.Context, _ [32]byte, _ time.Time, _ time.Duration) (login.Session, error) {
	f.lookupCalls.Add(1)
	if f.entered != nil {
		f.entered <- struct{}{}
		select {
		case <-f.release:
		case <-ctx.Done():
			return login.Session{}, ctx.Err()
		}
		return login.Session{}, login.ErrSession
	}
	return f.session, nil
}
func (f *h4Repository) SaveLogin(context.Context, login.SessionInput) (login.Session, error) {
	if f.saveCalls.Add(1) > 1 && f.rotate {
		return login.Session{}, login.ErrSession
	}
	return f.session, nil
}

func h4Fixture(t *testing.T, configure ...func(*login.Config)) (*platform.Router, *login.Service, *h4Repository, string) {
	t.Helper()
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(secret)
	salt := secret[:16]
	digest := argon2.IDKey([]byte(input), salt, 2, 19456, 1, 32)
	format := "$argon2id$v=19$m=19456,t=2,p=1$%s$%s"
	phc := fmt.Sprintf(format, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(digest))
	now := time.Now()
	f := &h4Repository{fakeRepo: fakeRepo{credential: login.Credential{UserID: "user", Status: "active", PasswordHash: phc}, session: login.Session{DatabaseNow: now, ID: "session", UserID: "user", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}}}
	cfg := login.Config{Origin: "https://admin.example.test", SigningKey: secretKey(t)}
	for _, change := range configure {
		change(&cfg)
	}
	s, err := login.New(f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, "https://admin.example.test", 30*time.Minute)
	r := platform.New(h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, route := range h.Routes() {
		if err := r.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	return r, s, f, input
}

func secretKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestH4SessionFloodBoundsDatabaseAndLeavesLoginAvailable(t *testing.T) {
	r, s, f, input := h4Fixture(t)
	f.entered, f.release = make(chan struct{}, 8), make(chan struct{})
	defer close(f.release)
	raw := base64.RawURLEncoding.EncodeToString(secretKey(t))
	for i := 0; i < 8; i++ {
		done := make(chan int, 1)
		go func() {
			done <- request(r, "GET", "/api/v1/auth/session", "", &http.Cookie{Name: SessionCookie, Value: raw}).Code
		}()
		select {
		case <-f.entered:
		case code := <-done:
			if code != 429 {
				t.Errorf("excess query status = %d", code)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("session request queued without admission decision")
		}
	}
	q := signedLogin(t, s, h4Email, input)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, q)
	if w.Code != 201 {
		t.Fatalf("owner login under session flood = %d", w.Code)
	}
	if n := f.lookupCalls.Load(); n > 2 {
		t.Fatalf("unbounded session DB work: %d entered, want <= 2", n)
	}
}

func TestH4SessionAdmittedOnce(t *testing.T) {
	r, _, f, _ := h4Fixture(t)
	raw := base64.RawURLEncoding.EncodeToString(secretKey(t))
	w := request(r, "GET", "/api/v1/auth/session", "", &http.Cookie{Name: SessionCookie, Value: raw})
	if w.Code != 200 || f.lookupCalls.Load() != 1 {
		t.Fatalf("session status=%d database lookups=%d", w.Code, f.lookupCalls.Load())
	}
}

func TestH4RotationLoserCannotDeleteWinnerCookie(t *testing.T) {
	r, s, f, input := h4Fixture(t)
	f.rotate = true
	raw := base64.RawURLEncoding.EncodeToString(secretKey(t))
	start := make(chan struct{})
	done := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		q := signedLogin(t, s, h4Email, input)
		q.AddCookie(&http.Cookie{Name: SessionCookie, Value: raw})
		go func() { <-start; w := httptest.NewRecorder(); r.ServeHTTP(w, q); done <- w }()
	}
	close(start)
	winners, losers := 0, 0
	for i := 0; i < 2; i++ {
		select {
		case w := <-done:
			switch w.Code {
			case 201:
				winners++
			case 401:
				losers++
				if len(w.Result().Cookies()) != 0 {
					t.Error("late losing rotation overwrites the winning browser cookie")
				}
			default:
				t.Errorf("rotation status %d", w.Code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("rotation did not complete")
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("rotation outcomes winners=%d losers=%d", winners, losers)
	}
}

func TestH4MalformedSessionNeverReachesDatabase(t *testing.T) {
	r, _, f, _ := h4Fixture(t)
	for _, raw := range []string{"", "invalid", strings.Repeat("A", 44), strings.Repeat("/", 43)} {
		w := request(r, "GET", "/api/v1/auth/session", "", &http.Cookie{Name: SessionCookie, Value: raw})
		if w.Code != 401 {
			t.Errorf("malformed cookie status %d", w.Code)
		}
	}
	if f.lookupCalls.Load() != 0 {
		t.Fatal("malformed session reached DB")
	}
}
