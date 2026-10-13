// Responsibilities: canonical opaque token hashes and fixed Argon2id PHC work.
// Dependencies: CSPRNG/SHA256/constant-time/base64 and injected Argon2 IDKey;
// auth commands call these helpers. Invariants: hostile PHC parameters never
// reach the KDF, and a deadline never releases a worker's real occupied slot.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"strings"
)

const phcPrefix = "$argon2id$v=19$m=19456,t=2,p=1$"
const dummyPHC = phcPrefix + "MDEyMzQ1Njc4OWFiY2RlZg$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func newToken() (string, [32]byte, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", [32]byte{}, c.ErrUnavailable
	}
	return base64.RawURLEncoding.EncodeToString(bytes[:]), sha256.Sum256(bytes[:]), nil
}
func tokenHash(raw string) ([32]byte, bool) {
	if len(raw) != 43 {
		return [32]byte{}, false
	}
	bytes, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(bytes) != 32 || base64.RawURLEncoding.EncodeToString(bytes) != raw {
		return [32]byte{}, false
	}
	return sha256.Sum256(bytes), true
}
func decodePHC(phc string) ([]byte, []byte, bool) {
	if len(phc) > 128 || !strings.HasPrefix(phc, phcPrefix) {
		return nil, nil, false
	}
	parts := strings.Split(strings.TrimPrefix(phc, phcPrefix), "$")
	if len(parts) != 2 {
		return nil, nil, false
	}
	salt, e1 := base64.RawStdEncoding.Strict().DecodeString(parts[0])
	hash, e2 := base64.RawStdEncoding.Strict().DecodeString(parts[1])
	if e1 != nil || e2 != nil || len(salt) != 16 || len(hash) != 32 || base64.RawStdEncoding.EncodeToString(salt) != parts[0] || base64.RawStdEncoding.EncodeToString(hash) != parts[1] {
		return nil, nil, false
	}
	return salt, hash, true
}
func (s *Service) derive(ctx context.Context, password string, salt []byte) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, rateLimited(0)
	}
	select {
	case s.hashSlots <- struct{}{}:
	default:
		return nil, rateLimited(0)
	}
	result := make(chan []byte, 1)
	derive := s.deriveKey
	go func() {
		// IDKey is non-preemptible. The buffered result avoids abandoned worker
		// sends; its slot remains occupied until the computation really ends.
		actual := derive([]byte(password), salt, 2, 19456, 1, 32)
		<-s.hashSlots
		result <- actual
	}()
	select {
	case hash := <-result:
		if ctx.Err() != nil {
			return nil, rateLimited(0)
		}
		if len(hash) != 32 {
			return nil, c.ErrUnavailable
		}
		return hash, nil
	case <-ctx.Done():
		return nil, rateLimited(0)
	}
}
func (s *Service) hashPassword(ctx context.Context, password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", c.ErrUnavailable
	}
	hash, err := s.derive(ctx, password, salt)
	if err != nil {
		return "", err
	}
	return phcPrefix + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash), nil
}
func (s *Service) verifyPassword(ctx context.Context, password, phc string, eligible bool) (bool, error) {
	salt, expected, valid := decodePHC(phc)
	if !eligible || !valid {
		salt, expected, _ = decodePHC(dummyPHC)
	}
	actual, err := s.derive(ctx, password, salt)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1 && eligible && valid, nil
}
