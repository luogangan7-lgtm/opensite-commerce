// Package service owns offline owner-bootstrap validation and password hashing.
// Dependencies: Argon2 and a sqlc-backed store consumer; called only by identity's
// public port. Invariants: no plaintext persistence and no dependency errors escape.
package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

type Request struct{ DatabaseURL, Email, TenantName, Password string }

var errBootstrap = errors.New("bootstrap failed")

func Bootstrap(ctx context.Context, input Request, persist func(context.Context, string, string, string, string) (string, error)) (string, error) {
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	input.TenantName = strings.TrimSpace(input.TenantName)
	address, err := mail.ParseAddress(input.Email)
	at := strings.LastIndexByte(input.Email, '@')
	if err != nil || address.Address != input.Email || len(input.Email) > 254 || at < 1 || !strings.Contains(input.Email[at+1:], ".") || strings.ContainsAny(input.Email, "\r\n\x00") || strings.TrimSpace(input.DatabaseURL) == "" || input.TenantName == "" || !utf8.ValidString(input.TenantName) || utf8.RuneCountInString(input.TenantName) > 200 || strings.IndexFunc(input.TenantName, unicode.IsControl) >= 0 || len(input.Password) < 12 || len(input.Password) > 1024 || strings.ContainsAny(input.Password, "\r\n\x00") || ctx.Err() != nil {
		return "", errBootstrap
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", errBootstrap
	}
	hash := argon2.IDKey([]byte(input.Password), salt, 2, 19456, 1, 32)
	phc := fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash))
	if ctx.Err() != nil {
		return "", errBootstrap
	}
	id, err := persist(ctx, input.DatabaseURL, input.Email, input.TenantName, phc)
	if err != nil || ctx.Err() != nil {
		return "", errBootstrap
	}
	return id, nil
}
