// Responsibility: pure admission boundaries before any mapped transaction.
// Dependencies: stdlib, module verifier; CI calls these unit assertions.
// Invariants: exact raw-byte MAC, strict flat envelope, no payload tenant authority.
package payments

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
)

func TestRawAdmissionBeforeTransaction(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	called := false
	m, err := New(Config{Key: key, Run: func(context.Context, string, func(context.Context, pgx.Tx, string, time.Time) error) error {
		called = true
		return nil
	}, Apply: func(context.Context, pgx.Tx, payment.VerifiedEvent) (bool, error) {
		panic("unexpected fact application")
	}})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"event_id":"event-1","account_id":"acct_test","order_id":"10000000-0000-4000-8000-000000000001","order_version":1,"provider_ref":"fake_1","status":"captured","amount_minor":100,"currency":"USD"}`)
	sign := func(body []byte) string {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte("1700000000."))
		mac.Write(body)
		return "t=1700000000,v1=" + hex.EncodeToString(mac.Sum(nil))
	}
	if in, stamp, err := m.verify(raw, sign(raw)); err != nil || stamp != 1700000000 || in.AccountID != "acct_test" {
		t.Fatalf("valid authenticated envelope: %v", err)
	}
	cases := []struct {
		name      string
		body      []byte
		signature string
	}{
		{"missing signature", raw, ""},
		{"timestamp-free MAC", raw, strings.Split(sign(raw), "v1=")[1]},
		{"malformed MAC", raw, "t=1700000000,v1=00"},
		{"modified exact bytes", append(bytes.Clone(raw), ' '), sign(raw)},
		{"too large", bytes.Repeat([]byte("x"), (64<<10)+1), ""},
	}
	for _, body := range []string{
		strings.Replace(string(raw), `"account_id":"acct_test"`, `"account_id":"acct_test","tenant_id":"10000000-0000-4000-8000-000000000002"`, 1),
		strings.Replace(string(raw), `"account_id":"acct_test"`, `"account_id":"acct_test","account_id":"acct_other"`, 1),
		strings.Replace(string(raw), `"captured"`, `"authorized"`, 1),
		strings.Replace(string(raw), `"amount_minor":100`, `"amount_minor":100.5`, 1),
		strings.Replace(string(raw), `"amount_minor":100`, `"amount_minor":-1`, 1),
		strings.Replace(string(raw), `"account_id":"acct_test"`, `"account_id":"../acct"`, 1),
		string(raw) + `{}`,
	} {
		cases = append(cases, struct {
			name      string
			body      []byte
			signature string
		}{fmt.Sprintf("shape-%d", len(cases)), []byte(body), sign([]byte(body))})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			if _, err := m.Receive(context.Background(), tc.body, tc.signature); err == nil {
				t.Fatal("invalid envelope accepted")
			}
			if called {
				t.Fatal("invalid envelope reached database")
			}
		})
	}
}
