// Responsibility: offline TLS peer and negative admission tests for test-only SMTP.
// Dependencies: stdlib TLS/SMTP protocol; never contacts an external host.
// Invariants: real certificate verification, exact destinations and no retry after DATA.
package service

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
)

const externalCanary = "SECRET-CANARY-not-a-real-credential"

func externalEnv() map[string]string {
	return map[string]string{"SMTP_TEST_HOST": "smtp.qq.com", "SMTP_TEST_PORT": "465", "SMTP_TEST_USER": "owner@qq.com", "SMTP_TEST_AUTH_CODE": externalCanary, "SMTP_TEST_FROM": "owner@qq.com", "SMTP_TEST_TO": "allowed@example.com,second@example.com", "APP_ENV": "test"}
}

func externalSender(t *testing.T, env map[string]string) *ExternalTestSMTP {
	t.Helper()
	s, err := ExternalTestSenderFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func externalMessage() contract.Message {
	return contract.Message{Key: strings.Repeat("a", 64), Recipient: "allowed@example.com", Subject: "Fake test", Text: "fake", HTML: "<p>fake</p>"}
}

func TestExternalSMTPAdmissionBeforeDial(t *testing.T) {
	for _, change := range []map[string]string{
		{"SMTP_TEST_PORT": ""}, {"SMTP_TEST_PORT": "25"}, {"SMTP_TEST_PORT": "587"}, {"SMTP_TEST_PORT": "0465"}, {"SMTP_TEST_PORT": "465 "},
		{"SMTP_TEST_HOST": "mail.spacemail.com.evil.example"}, {"SMTP_TEST_HOST": "MAIL.SPACEMAIL.COM"}, {"SMTP_TEST_HOST": "mail.spacemail.com:465"},
		{"SMTP_TEST_HOST": "evil.example"}, {"SMTP_TEST_HOST": "SMTP.QQ.COM"}, {"SMTP_TEST_HOST": "smtp.qq.com:465"},
		{"APP_ENV": "production"}, {"APP_ENV": " PRODUCTION "}, {"ENVIRONMENT": "production"}, {"SMTP_TEST_FROM": "other@qq.com"},
		{"SMTP_TEST_TO": ""}, {"SMTP_TEST_TO": "Name <allowed@example.com>"}, {"SMTP_TEST_TO": "allowed@example.com,"},
		{"SMTP_TEST_AUTH_CODE": ""}, {"SMTP_TEST_USER": "bad\r\nuser"},
	} {
		env := externalEnv()
		for k, v := range change {
			env[k] = v
		}
		if _, err := ExternalTestSenderFromEnv(func(k string) string { return env[k] }); err == nil || strings.Contains(err.Error(), externalCanary) {
			t.Fatal("EXTERNAL_CONFIG_MUST_FAIL_CLOSED")
		}
	}
	s := externalSender(t, externalEnv())
	var calls atomic.Int32
	s.dialContext = func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New(externalCanary)
	}
	for _, address := range []string{"outsider@example.com", "Allowed@example.com", "allowed@example.com,outsider@example.com", "Name <allowed@example.com>", "allowed@example.com\r\nBcc: outsider@example.com"} {
		m := externalMessage()
		m.Recipient = address
		if err := s.Send(context.Background(), m); err == nil {
			t.Fatal("EXTERNAL_ALLOWLIST_BEFORE_DIAL")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("EXTERNAL_REJECT_DIALED")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Send(ctx, externalMessage()); err == nil || calls.Load() != 0 {
		t.Fatal("EXTERNAL_CANCELED_DIALED")
	}
	if err := s.Send(context.Background(), externalMessage()); err == nil || strings.Contains(err.Error(), externalCanary) {
		t.Fatal("EXTERNAL_DIAL_SECRET_LEAK")
	}
}

func externalCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "offline test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"smtp.qq.com", "mail.spacemail.com"}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, parsed, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der, caDER}, PrivateKey: key}, pool
}

func TestExternalSMTPProtocol(t *testing.T) {
	cert, roots := externalCertificate(t)
	for _, host := range []string{"smtp.qq.com", "mail.spacemail.com"} {
		for _, scenario := range []string{"sent", "ambiguous", "auth-error", "untrusted", "plaintext", "starttls", "cancel"} {
			t.Run(host+"/"+scenario, func(t *testing.T) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				done := make(chan struct{})
				var wire strings.Builder
				var peerErr error
				go func() {
					defer close(done)
					conn, e := listener.Accept()
					if e != nil {
						return
					}
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
					if scenario == "plaintext" || scenario == "starttls" {
						_, _ = io.WriteString(conn, "220 plaintext STARTTLS\r\n")
						_, _ = io.Copy(io.Discard, conn)
						return
					}
					if scenario == "cancel" {
						_, _ = io.Copy(io.Discard, conn)
						return
					}
					tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
					if tc.Handshake() != nil {
						return
					}
					if tc.ConnectionState().ServerName != host {
						peerErr = errors.New("EXTERNAL_SNI_REQUIRED")
						return
					}
					r := bufio.NewReader(tc)
					send := func(s string) { _, _ = io.WriteString(tc, s+"\r\n") }
					send("220 fake SMTP")
					for {
						line, e := r.ReadString('\n')
						if e != nil {
							return
						}
						switch {
						case strings.HasPrefix(line, "EHLO"):
							send("250-fake")
							send("250 AUTH PLAIN")
						case strings.HasPrefix(line, "AUTH PLAIN"):
							payload := strings.TrimSpace(strings.TrimPrefix(line, "AUTH PLAIN "))
							decoded, decodeErr := base64.StdEncoding.DecodeString(payload)
							if decodeErr != nil || string(decoded) != "\x00owner@qq.com\x00"+externalCanary {
								peerErr = errors.New("EXTERNAL_TLS_AUTH_REQUIRED")
								return
							}
							if scenario == "auth-error" {
								send("535 " + externalCanary)
								return
							}
							send("235 ok")
						case strings.HasPrefix(line, "MAIL FROM:<owner@qq.com>"), strings.HasPrefix(line, "RCPT TO:<allowed@example.com>"):
							send("250 ok")
						case strings.HasPrefix(line, "DATA"):
							send("354 continue")
							for {
								line, e = r.ReadString('\n')
								if e != nil {
									return
								}
								if line == ".\r\n" {
									break
								}
								wire.WriteString(line)
							}
							if scenario == "ambiguous" {
								return
							}
							send("250 accepted")
						case strings.HasPrefix(line, "QUIT"):
							send("221 bye")
							return
						default:
							peerErr = errors.New("UNEXPECTED_SMTP_COMMAND")
							send("500 rejected")
							return
						}
					}
				}()
				env := externalEnv()
				env["SMTP_TEST_HOST"] = host
				s := externalSender(t, env)
				if scenario != "untrusted" {
					s.testRoots = roots
				}
				var calls atomic.Int32
				s.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
					if address != net.JoinHostPort(host, "465") {
						return nil, errors.New("EXTERNAL_PORT_NOT_465")
					}
					calls.Add(1)
					return (&net.Dialer{}).DialContext(ctx, network, listener.Addr().String())
				}
				limit := 2 * time.Second
				if scenario == "cancel" {
					limit = 100 * time.Millisecond
				}
				ctx, cancel := context.WithTimeout(context.Background(), limit)
				defer cancel()
				err = s.Send(ctx, externalMessage())
				<-done
				if calls.Load() != 1 || peerErr != nil {
					t.Fatal("EXTERNAL_RETRY_OR_PROTOCOL", peerErr)
				}
				if err != nil && strings.Contains(err.Error(), externalCanary) {
					t.Fatal("EXTERNAL_SECRET_LEAK")
				}
				var ambiguous AmbiguousError
				if scenario == "sent" {
					if err != nil || !strings.Contains(wire.String(), "From: owner@qq.com\r\n") || !strings.Contains(wire.String(), "multipart/alternative") || !strings.Contains(wire.String(), "Message-ID: <"+externalMessage().Key) {
						t.Fatal("EXTERNAL_MIME_OR_SEND", err)
					}
				} else if scenario == "ambiguous" {
					if !errors.As(err, &ambiguous) {
						t.Fatal("EXTERNAL_DATA_MUST_BE_UNCERTAIN", err)
					}
				} else if err == nil || errors.As(err, &ambiguous) {
					t.Fatal("EXTERNAL_TLS_OR_AUTH_MUST_FAIL", err)
				}
			})
		}
	}
}
