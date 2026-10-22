// Responsibilities: pin unified staff admission and the sealed self projection.
// Dependencies: C02c router fixtures; CI runs this with the identity suite.
// Invariants: one lookup, validated client IP, shared IPv6 /64 and global limits.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func TestH4ASessionConcurrencyAndDeadline(t *testing.T) {
	_, _, repo, service, token := transportFixture(t)
	repo.lookupBlock = make(chan struct{}, 2)
	results := make(chan error, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	for i := 0; i < cap(results); i++ {
		go func() {
			_, _, err := service.Authenticate(ctx, token)
			results <- err
		}()
		<-repo.lookupBlock
	}
	if _, _, err := service.Authenticate(ctx, token); !errors.Is(err, login.ErrRateLimited) {
		t.Fatal("third concurrent lookup must be rejected before storage")
	}
	for i := 0; i < cap(results); i++ {
		if err := <-results; !errors.Is(err, login.ErrDatabase) || time.Since(started) > 2500*time.Millisecond {
			t.Fatal("admitted lookup ignored internal deadline", err)
		}
	}
}

func TestH4ASelfReadsSessionOnce(t *testing.T) {
	_, router, repo, _, token := transportFixture(t)
	w := request(router, "GET", "/api/v1/auth/session", "", &http.Cookie{Name: SessionCookie, Value: token})
	if w.Code != 200 || repo.lookups != 1 {
		t.Fatalf("self status=%d lookups=%d; want 200 and exactly one", w.Code, repo.lookups)
	}
}

func TestH4AUnifiedSessionAdmission(t *testing.T) {
	for _, mode := range []string{"invalid_ip", "ipv6", "global", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			f := c02cAuthFixture(t)
			for i := 0; i <= 600; i++ {
				r := f.request("GET", c02cTenant, true, false)
				r.SetPathValue("tenant_id", c02cTenant)
				r.RemoteAddr = fmt.Sprintf("198.18.%d.%d:1234", i/250, i%250+1)
				want := 200
				switch mode {
				case "invalid_ip":
					r.RemoteAddr, want = "invalid", 401
				case "ipv6":
					r.RemoteAddr = fmt.Sprintf("[2001:db8:1:2::%x]:1234", i+1)
					if i >= 60 {
						want = 429
					}
				case "global":
					if i >= 600 {
						want = 429
					}
				case "malformed":
					r.Header.Set("Cookie", SessionCookie+"=malformed")
					want = 401
				}
				ctx, err := f.handler.Authenticate(r.Context(), r, c02cSpec(platform.AdminOrAPIKey, "GET", "catalog:read"))
				got := 200
				if err != nil {
					w := httptest.NewRecorder()
					platform.WriteProblem(w, r, err)
					got = w.Code
				}
				if got != want {
					t.Fatalf("%s request %d status=%d want=%d", mode, i, got, want)
				}
				if err == nil {
					if _, ok := AuthorizationFromContext(ctx); !ok {
						t.Fatal("missing principal")
					}
				}
			}
			if (mode == "invalid_ip" || mode == "malformed") && f.sessions.lookups != 0 {
				t.Fatal("cheap reject reached storage")
			}
		})
	}
}
