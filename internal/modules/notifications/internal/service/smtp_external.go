// Responsibility: isolated, owner-gated external SMTP smoke-test transport.
// Dependencies: stdlib TLS/SMTP and shared MIME; only the test command composes it.
// Invariants: QQ:465, verified TLS before AUTH, exact allowlist, no production,
// safe errors and one attempt only; uncertain DATA requires manual investigation.
package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
)

type ExternalTestSMTP struct {
	host, user, password, from string
	allowed map[string]bool
	// Unexported seams are assigned only by offline tests, never by env/config.
	dialContext func(context.Context, string, string) (net.Conn, error)
	testRoots *x509.CertPool
}

func ExternalTestSenderFromEnv(get func(string) string) (*ExternalTestSMTP, error) {
	invalid := func() (*ExternalTestSMTP, error) { return nil, errors.New("NOTIFICATION_EXTERNAL_TEST_CONFIG_INVALID") }
	if get == nil { return invalid() }
	if strings.EqualFold(strings.TrimSpace(get("APP_ENV")), "production") || strings.EqualFold(strings.TrimSpace(get("ENVIRONMENT")), "production") { return invalid() }
	host, user, password := get("SMTP_TEST_HOST"), get("SMTP_TEST_USER"), get("SMTP_TEST_AUTH_CODE")
	from := get("SMTP_TEST_FROM")
	if host != "smtp.qq.com" || !ValidRecipient(user) || from != user || password == "" || len(password) > 4096 || strings.ContainsAny(password, "\r\n") { return invalid() }
	allowed := make(map[string]bool)
	for _, address := range strings.Split(get("SMTP_TEST_TO"), ",") {
		address = strings.TrimSpace(address)
		if !ValidRecipient(address) { return invalid() }
		allowed[address] = true
	}
	return &ExternalTestSMTP{host: host, user: user, password: password, from: from, allowed: allowed}, nil
}

func (s *ExternalTestSMTP) Send(ctx context.Context, m contract.Message) error {
	failed := func() error { return errors.New("NOTIFICATION_EXTERNAL_TEST_SEND_FAILED") }
	if s == nil || !s.allowed[m.Recipient] || !ValidRecipient(m.Recipient) || s.from != s.user || s.host != "smtp.qq.com" { return failed() }
	if ctx.Err() != nil { return failed() }
	body, err := mimeEnvelope(m, s.from)
	if err != nil { return failed() }
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	dial := s.dialContext
	if dial == nil { dial = (&net.Dialer{Timeout: 10*time.Second}).DialContext }
	raw, err := dial(ctx, "tcp", net.JoinHostPort(s.host, "465"))
	if err != nil { return failed() }
	defer raw.Close()
	deadline, _ := ctx.Deadline()
	if raw.SetDeadline(deadline) != nil { return failed() }
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() }); defer stop()
	conn := tls.Client(raw, &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12, RootCAs: s.testRoots})
	if conn.HandshakeContext(ctx) != nil { return failed() }
	client, err := smtp.NewClient(conn, s.host)
	if err != nil { return failed() }
	defer client.Close()
	if client.Auth(smtp.PlainAuth("", s.user, s.password, s.host)) != nil { return failed() }
	if client.Mail(s.from) != nil { return failed() }
	if client.Rcpt(m.Recipient) != nil { return failed() }
	writer, err := client.Data()
	if err != nil { return failed() }
	// Do not retry once DATA was admitted, even if the write or final ack fails.
	if _, err = writer.Write(body); err != nil { return AmbiguousError{} }
	if writer.Close() != nil { return AmbiguousError{} }
	_ = client.Quit()
	return nil
}
