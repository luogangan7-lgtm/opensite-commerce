// Responsibility: local SMTP configuration and MIME safety gate.
// Dependencies: stdlib; production credentials and real destinations absent.
package service

import (
	"bufio"
	"context"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSMTPConfigFailClosed(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	s, e := SenderFromEnv(get)
	if e != nil || s != nil {
		t.Fatal("NOTIFICATION_SMTP_DEFAULT_OFF")
	}
	env["NOTIFICATIONS_SMTP_ENABLED"] = "true"
	env["NOTIFICATIONS_SMTP_HOST"] = "127.0.0.1"
	env["NOTIFICATIONS_SMTP_PORT"] = "2525"
	if _, e = SenderFromEnv(get); e != nil {
		t.Fatal(e)
	}
	for _, host := range []string{"example.com", "0.0.0.0", "127.0.0.1.attacker.test", "http://localhost", "localhost:25"} {
		env["NOTIFICATIONS_SMTP_HOST"] = host
		if _, e = SenderFromEnv(get); e == nil {
			t.Fatal("NOTIFICATION_SMTP_NONLOOPBACK", host)
		}
	}
	env["NOTIFICATIONS_SMTP_HOST"] = "localhost"
	env["APP_ENV"] = "production"
	if _, e = SenderFromEnv(get); e == nil {
		t.Fatal("NOTIFICATION_SMTP_PRODUCTION")
	}
}
func TestMIMEHeadersAndStableID(t *testing.T) {
	m := contract.Message{Key: strings.Repeat("a", 64), Recipient: "a@safe.test", Subject: "付款已收到", Text: "hello", HTML: "<p>hello</p>"}
	b, e := MIME(m)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(b), "Message-ID: <"+m.Key+"@notifications.safe.test>") || !strings.Contains(string(b), "multipart/alternative") {
		t.Fatal("NOTIFICATION_MIME")
	}
	m.Recipient = "real@example.com"
	if _, e = MIME(m); e == nil {
		t.Fatal("NOTIFICATION_SMTP_REAL_RECIPIENT")
	}
	m.Recipient = "a@safe.test"
	m.Subject = "x\r\nBcc: y@safe.test"
	if _, e = MIME(m); e == nil {
		t.Fatal("NOTIFICATION_HEADER_INJECTION")
	}
}

// A loopback protocol peer proves classification at the real DATA boundary.
func TestSMTPProtocolOutcomes(t *testing.T) {
	for _, scenario := range []string{"sent", "before-data", "after-data"} {
		t.Run(scenario, func(t *testing.T) {
			listener, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer listener.Close()
			done := make(chan struct{})
			var wire string
			go func() {
				defer close(done)
				conn, e := listener.Accept()
				if e != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				r := bufio.NewReader(conn)
				send := func(s string) { _, _ = io.WriteString(conn, s+"\r\n") }
				send("220 local.safe.test")
				for {
					line, e := r.ReadString('\n')
					if e != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
						send("250 local.safe.test")
					case strings.HasPrefix(line, "MAIL"), strings.HasPrefix(line, "RCPT"):
						send("250 OK")
					case strings.HasPrefix(line, "DATA"):
						if scenario == "before-data" {
							send("554 rejected")
							continue
						}
						send("354 send body")
						for {
							line, e = r.ReadString('\n')
							if e != nil {
								return
							}
							if line == ".\r\n" {
								break
							}
							wire += line
						}
						if scenario == "after-data" {
							return
						}
						send("250 accepted")
					case strings.HasPrefix(line, "QUIT"):
						send("221 bye")
						return
					default:
						send("250 OK")
					}
				}
			}()
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			sender, e := SenderFromEnv(func(k string) string {
				switch k {
				case "NOTIFICATIONS_SMTP_ENABLED":
					return "true"
				case "NOTIFICATIONS_SMTP_HOST":
					return "localhost"
				case "NOTIFICATIONS_SMTP_PORT":
					return port
				}
				return ""
			})
			if e != nil {
				t.Fatal(e)
			}
			m := contract.Message{Key: strings.Repeat("b", 64), Recipient: "buyer@safe.test", Subject: "付款", Text: "plain", HTML: "<p>html</p>"}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			e = sender.Send(ctx, m)
			<-done
			var uncertain AmbiguousError
			switch scenario {
			case "sent":
				if e != nil || !strings.Contains(wire, "Message-ID: <"+m.Key+"@notifications.safe.test>") || !strings.Contains(wire, "quoted-printable") {
					t.Fatal("NOTIFICATION_SMTP_ACCEPT", e)
				}
			case "before-data":
				if e == nil || errors.As(e, &uncertain) {
					t.Fatal("NOTIFICATION_SMTP_KNOWN_FAILURE", e)
				}
			case "after-data":
				if !errors.As(e, &uncertain) {
					t.Fatal("NOTIFICATION_SMTP_ACK_UNCERTAIN", e)
				}
			}
		})
	}
}
func TestSMTPHonorsCancellation(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.Copy(io.Discard, conn)
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	s := &SMTP{address: net.JoinHostPort("127.0.0.1", port), host: "127.0.0.1"}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	e = s.Send(ctx, contract.Message{Key: strings.Repeat("c", 64), Recipient: "a@safe.test", Subject: "s", Text: "t", HTML: "h"})
	if e == nil || time.Since(start) > time.Second {
		t.Fatal("NOTIFICATION_SMTP_CONTEXT", e)
	}
	<-done
}
