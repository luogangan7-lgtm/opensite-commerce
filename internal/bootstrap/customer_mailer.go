// Responsibility: adapt customer email challenges to the public notification sender.
// Dependencies: public customer/notifications contracts; bootstrap supplies tokens.
// Invariants: local test recipients only, pinned HTTPS origin, token only in fragment;
// no transport errors or challenge secrets escape through returned errors.
package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"html"
	"net/mail"
	"net/url"
	"regexp"
	"strings"

	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

type customerMailer struct {
	sender contract.Sender
	origin string
}

// newCustomerMailer pins the storefront origin separately from request input.
func newCustomerMailer(sender contract.Sender, origin string) (*customerMailer, error) {
	u, err := url.Parse(origin)
	if sender == nil || err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(origin, "\r\n\\") {
		return nil, c.ErrInvalidInput
	}
	return &customerMailer{sender: sender, origin: origin}, nil
}

var localMailAddress = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+/=?^_` + "`" + `{|}~-]+([.][A-Za-z0-9!#$%&'*+/=?^_` + "`" + `{|}~-]+)*@[A-Za-z0-9-]+([.][A-Za-z0-9-]+)*$`)

// Send never persists the rendered token. Browser-only fragments avoid server
// request logs and referrers; the future form must consume the token by POST.
func (m *customerMailer) Send(ctx context.Context, tenant, email, purpose, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.sender == nil {
		return c.ErrUnavailable
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	address, addressErr := mail.ParseAddress(email)
	domain := strings.ToLower(email[strings.LastIndexByte(email, '@')+1:])
	if !tenanttx.ValidTenantID(tenant) || tenant == "00000000-0000-0000-0000-000000000000" || len(email) > 254 || !localMailAddress.MatchString(email) || addressErr != nil || address.Address != email || !(domain == "localhost" || strings.HasSuffix(domain, ".test") || strings.HasSuffix(domain, ".invalid")) || err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != token {
		return c.ErrInvalidInput
	}
	path, subject := "/customer/verify", "Verify your email address"
	switch purpose {
	case "register":
	case "reset":
		path, subject = "/customer/reset", "Reset your password"
	default:
		return c.ErrInvalidInput
	}
	link := m.origin + path + "#" + url.Values{"tenant": {tenant}, "token": {token}}.Encode()
	key := sha256.Sum256([]byte(tenant + "\x00" + purpose + "\x00" + token))
	message := contract.Message{
		Key: hex.EncodeToString(key[:]), Recipient: email, Subject: subject,
		Text: subject + " using this single-use link:\n" + link,
		HTML: "<p>" + html.EscapeString(subject) + " using this single-use link:</p><p><a href=\"" + html.EscapeString(link) + "\">" + html.EscapeString(subject) + "</a></p>",
	}
	if err := m.sender.Send(ctx, message); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return c.ErrUnavailable
	}
	return nil
}
