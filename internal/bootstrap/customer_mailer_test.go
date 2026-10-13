// Responsibility: exercise the customer mail adapter through its public interface.
// Dependencies: stdlib and notifications/contract; CI owns execution, no real email.
// Invariants: challenges never enter URL paths/queries, untrusted inputs fail closed.
package bootstrap

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"testing"

	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
)

const mailTenant = "11111111-1111-4111-8111-111111111111"

// mailCapture is an in-memory sender, never a transport or a production mailbox.
type mailCapture struct {
	messages []contract.Message
	err      error
}

func (s *mailCapture) Send(ctx context.Context, message contract.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.err != nil {
		return s.err
	}
	s.messages = append(s.messages, message)
	return nil
}

func challengeToken() string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("v", 32)))
}

func TestNotificationMailerChallengeLinksAndKeys(t *testing.T) {
	sender := &mailCapture{}
	mailer, err := newCustomerMailer(sender, "https://store.example.test")
	if err != nil {
		t.Fatal("valid origin rejected")
	}
	var port c.Mailer = mailer
	for _, purpose := range []string{"register", "register", "reset"} {
		if err := port.Send(context.Background(), mailTenant, "buyer@example.test", purpose, challengeToken()); err != nil {
			t.Fatal("valid local challenge must reach sender")
		}
	}
	if len(sender.messages) != 3 {
		t.Fatal("challenge delivery missing")
	}
	if sender.messages[0].Key != sender.messages[1].Key || sender.messages[0].Key == sender.messages[2].Key {
		t.Fatal("challenge keys must be stable and purpose-separated")
	}
	for i, message := range sender.messages {
		key, err := hex.DecodeString(message.Key)
		if err != nil || len(key) != 32 || hex.EncodeToString(key) != message.Key {
			t.Fatal("invalid sender deduplication key")
		}
		if message.Recipient != "buyer@example.test" || message.Subject == "" {
			t.Fatal("missing challenge envelope")
		}
		var link string
		for _, field := range strings.Fields(message.Text) {
			if strings.HasPrefix(field, "https://") {
				link = field
			}
		}
		u, err := url.Parse(link)
		if err != nil || u.Scheme != "https" || u.Host != "store.example.test" || u.RawQuery != "" || u.User != nil {
			t.Fatal("challenge link must keep the pinned origin and empty query")
		}
		wantPath := "/customer/verify"
		if i == 2 {
			wantPath = "/customer/reset"
		}
		if u.Path != wantPath || strings.Contains(u.Path, challengeToken()) {
			t.Fatal("challenge path invalid")
		}
		fragment, err := url.ParseQuery(u.Fragment)
		if err != nil || fragment.Get("tenant") != mailTenant || fragment.Get("token") != challengeToken() {
			t.Fatal("challenge token must occur only in the browser fragment")
		}
		if !strings.Contains(message.HTML, "&amp;") || strings.Contains(message.HTML, "&token=") {
			t.Fatal("HTML challenge URL must escape its fragment separator")
		}
	}
}

func TestNotificationMailerRejectsUnsafeConfiguration(t *testing.T) {
	for _, origin := range []string{"", "http://store.example.test", "https://user:secret@store.example.test", "https://store.example.test/", "https://store.example.test/path", "https://store.example.test?query=secret", "https://store.example.test#secret", "https://store.example.test\r\nInjected: true"} {
		if _, err := newCustomerMailer(&mailCapture{}, origin); err == nil {
			t.Fatal("unsafe configured origin accepted")
		}
	}
	if _, err := newCustomerMailer(nil, "https://store.example.test"); err == nil {
		t.Fatal("missing sender accepted")
	}
}

func TestNotificationMailerRejectsUnsafeChallenges(t *testing.T) {
	for _, tc := range []struct{ tenant, email, purpose, token string }{
		{"bad", "buyer@example.test", "register", challengeToken()},
		{mailTenant, "buyer@example.com", "register", challengeToken()},
		{mailTenant, "Buyer <buyer@example.test>", "register", challengeToken()},
		{mailTenant, "buyer@example.test\r\nBcc: real@example.com", "register", challengeToken()},
		{mailTenant, "buyer@example..test", "register", challengeToken()},
		{mailTenant, "buyer@example.test", "login", challengeToken()},
		{mailTenant, "buyer@example.test", "register", "short"},
		{mailTenant, "buyer@example.test", "register", challengeToken() + "="},
	} {
		sender := &mailCapture{}
		mailer, err := newCustomerMailer(sender, "https://store.example.test")
		if err != nil {
			t.Fatal("test configuration rejected")
		}
		if err := mailer.Send(context.Background(), tc.tenant, tc.email, tc.purpose, tc.token); err == nil {
			t.Fatal("unsafe challenge accepted")
		}
		if len(sender.messages) != 0 {
			t.Fatal("unsafe challenge reached sender")
		}
	}
}

func TestNotificationMailerRedactsTransportFailureAndCancellation(t *testing.T) {
	sender := &mailCapture{err: errors.New("transport disclosed buyer@example.test " + challengeToken())}
	mailer, err := newCustomerMailer(sender, "https://store.example.test")
	if err != nil {
		t.Fatal("test configuration rejected")
	}
	err = mailer.Send(context.Background(), mailTenant, "buyer@example.test", "register", challengeToken())
	if err == nil || strings.Contains(err.Error(), challengeToken()) || strings.Contains(err.Error(), "buyer@example.test") {
		t.Fatal("transport failure must be redacted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := mailer.Send(ctx, mailTenant, "buyer@example.test", "register", challengeToken()); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled challenge must not be sent")
	}
}
