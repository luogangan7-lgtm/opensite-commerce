// Tests exercise bootstrap validation and independently decode/recompute the PHC.
// Dependency: service public function and Argon2; no database claims are made here.
package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"golang.org/x/crypto/argon2"
	"strings"
	"testing"
)

func TestBootstrapPHCAndRandomSalt(t *testing.T) {
	var hashes []string
	persist := func(ctx context.Context, dsn, email, tenant, phc string) (string, error) {
		if email != "owner@example.com" || tenant != "Store" {
			t.Fatal("input not normalized")
		}
		hashes = append(hashes, phc)
		return "owner-id", nil
	}
	for range 2 {
		id, err := Bootstrap(context.Background(), Request{DatabaseURL: "dsn", Email: " OWNER@example.com ", TenantName: " Store ", Password: "long-passphrase"}, persist)
		if err != nil || id != "owner-id" {
			t.Fatalf("bootstrap failed: %v", err)
		}
	}
	if hashes[0] == hashes[1] {
		t.Fatal("salt reused")
	}
	for _, phc := range hashes {
		parts := strings.Split(phc, "$")
		if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=19456,t=2,p=1" {
			t.Fatal("wrong PHC format")
		}
		salt, err := base64.RawStdEncoding.DecodeString(parts[4])
		if err != nil || len(salt) != 16 {
			t.Fatal("wrong salt")
		}
		hash, err := base64.RawStdEncoding.DecodeString(parts[5])
		if err != nil || len(hash) != 32 {
			t.Fatal("wrong digest")
		}
		expected := argon2.IDKey([]byte("long-passphrase"), salt, 2, 19456, 1, 32)
		if !bytes.Equal(hash, expected) {
			t.Fatal("independent Argon2 recompute differs")
		}
	}
}

func TestPersistenceErrorsAreRedacted(t *testing.T) {
	secret := "raw-driver-password-PHC-DSN"
	id, err := Bootstrap(context.Background(), Request{DatabaseURL: "dsn", Email: "owner@example.com", TenantName: "Store", Password: "long-passphrase"}, func(context.Context, string, string, string, string) (string, error) { return "", errors.New(secret) })
	if id != "" || err == nil || err.Error() != "bootstrap failed" || strings.Contains(err.Error(), secret) {
		t.Fatal("persistence error escaped service")
	}
}

func TestBootstrapRejectsInvalidInputsBeforePersist(t *testing.T) {
	good := Request{DatabaseURL: "dsn", Email: "owner@example.com", TenantName: "Store", Password: "long-passphrase"}
	cases := []Request{}
	for _, email := range []string{"", "Name <owner@example.com>", "a@b", strings.Repeat("a", 255) + "@x.com", "a\x00@x.com"} {
		x := good
		x.Email = email
		cases = append(cases, x)
	}
	for _, tenant := range []string{"", "   ", strings.Repeat("界", 201), "bad\nname", "bad\x00name"} {
		x := good
		x.TenantName = tenant
		cases = append(cases, x)
	}
	for _, password := range []string{"", "short", strings.Repeat("x", 1025), "long-pass\nword", "long-pass\x00word"} {
		x := good
		x.Password = password
		cases = append(cases, x)
	}
	x := good
	x.DatabaseURL = ""
	cases = append(cases, x)
	for i, input := range cases {
		_, err := Bootstrap(context.Background(), input, func(context.Context, string, string, string, string) (string, error) {
			t.Fatal("invalid input reached persist")
			return "", nil
		})
		if err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Bootstrap(ctx, good, func(context.Context, string, string, string, string) (string, error) {
		t.Fatal("cancelled call reached persist")
		return "", nil
	}); err == nil {
		t.Fatal("cancellation ignored")
	}
}
