// Responsibility: fail-closed local SMTP test adapter and multipart encoding.
// Dependencies: stdlib net/smtp; worker bootstrap supplies env configuration.
// Invariants: pinned loopback IP, test recipients, no secrets/raw network errors,
// ambiguous DATA acceptance becomes manual investigation, never an auto retry.
package service

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
)

type SMTP struct{ address, host, user, password string }

func SenderFromEnv(get func(string) string) (contract.Sender, error) {
	if get == nil {
		return nil, nil
	}
	enabled := get("NOTIFICATIONS_SMTP_ENABLED")
	if enabled == "" || enabled == "false" {
		return nil, nil
	}
	invalid := func() (contract.Sender, error) { return nil, errors.New("NOTIFICATION_SMTP_CONFIG_INVALID") }
	if enabled != "true" || get("APP_ENV") == "production" || get("ENVIRONMENT") == "production" {
		return invalid()
	}
	host := get("NOTIFICATIONS_SMTP_HOST")
	ip := net.ParseIP(host)
	if host == "localhost" {
		ip = net.ParseIP("127.0.0.1")
	} else if ip == nil || !ip.IsLoopback() {
		return invalid()
	}
	port, e := strconv.Atoi(get("NOTIFICATIONS_SMTP_PORT"))
	if e != nil || port < 1 || port > 65535 {
		return invalid()
	}
	user, password := get("NOTIFICATIONS_SMTP_USER"), get("NOTIFICATIONS_SMTP_PASSWORD")
	if (user == "") != (password == "") || len(user) > 256 || len(password) > 4096 || strings.ContainsAny(user+password, "\r\n") {
		return invalid()
	}
	return &SMTP{address: net.JoinHostPort(ip.String(), strconv.Itoa(port)), host: ip.String(), user: user, password: password}, nil
}

func MIME(m contract.Message) ([]byte, error) {
	key, e := hex.DecodeString(m.Key)
	if e != nil || len(key) != 32 || len(m.Subject) > 256 || strings.ContainsAny(m.Subject, "\r\n") || !validTestRecipient(m.Recipient) || len(m.Text) > 65536 || len(m.HTML) > 65536 {
		return nil, errors.New("NOTIFICATION_INVALID_ENVELOPE")
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, part := range []struct{ kind, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", part.kind+"; charset=utf-8")
		h.Set("Content-Transfer-Encoding", "quoted-printable")
		w, e := mw.CreatePart(h)
		if e != nil {
			return nil, e
		}
		qw := quotedprintable.NewWriter(w)
		if _, e = qw.Write([]byte(part.body)); e != nil {
			return nil, e
		}
		if e = qw.Close(); e != nil {
			return nil, e
		}
	}
	if e = mw.Close(); e != nil {
		return nil, e
	}
	header := fmt.Sprintf("From: notifications@safe.test\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <%s@notifications.safe.test>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=%q\r\n\r\n", m.Recipient, mime.QEncoding.Encode("utf-8", m.Subject), m.Key, mw.Boundary())
	return append([]byte(header), body.Bytes()...), nil
}
func (s *SMTP) Send(ctx context.Context, m contract.Message) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	b, e := MIME(m)
	if e != nil {
		return e
	}
	failed := func() error { return errors.New("NOTIFICATION_SEND_FAILED") }
	conn, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", s.address)
	if e != nil {
		return failed()
	}
	defer conn.Close()
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if e = conn.SetDeadline(deadline); e != nil {
		return failed()
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	c, e := smtp.NewClient(conn, s.host)
	if e != nil {
		return failed()
	}
	defer c.Close()
	if s.user != "" {
		if e = c.Auth(smtp.PlainAuth("", s.user, s.password, s.host)); e != nil {
			return failed()
		}
	}
	if e = c.Mail("notifications@safe.test"); e != nil {
		return failed()
	}
	if e = c.Rcpt(m.Recipient); e != nil {
		return failed()
	}
	w, e := c.Data()
	if e != nil {
		return failed()
	}
	// Once DATA is admitted, disconnects and uncertain 250 acknowledgments may
	// have delivered. Persist uncertainty even if the context expires here.
	if _, e = w.Write(b); e != nil {
		return AmbiguousError{}
	}
	if e = w.Close(); e != nil {
		return AmbiguousError{}
	}
	// QUIT failure cannot reverse an acknowledged DATA acceptance.
	_ = c.Quit()
	return nil
}

func validTestRecipient(s string) bool {
	if !ValidRecipient(s) {
		return false
	}
	domain := strings.ToLower(s[strings.LastIndexByte(s, '@')+1:])
	return domain == "localhost" || strings.HasSuffix(domain, ".test") || strings.HasSuffix(domain, ".invalid")
}
