// Cryptographic helpers own opaque tokens, domain-separated MACs and PHC checks.
// Dependencies: CSPRNG, SHA256, HMAC and Argon2; called only by login Service.
// Invariants: canonical 32-byte tokens; bounded fixed PHC; one hash per admitted attempt.
package login

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

const dummyPHC = "$argon2id$v=19$m=19456,t=2,p=1$MDEyMzQ1Njc4OWFiY2RlZg$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func newToken() (string, error) {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", ErrDatabase
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
func tokenHash(token string) ([32]byte, error) {
	b, e := base64.RawURLEncoding.Strict().DecodeString(token)
	if e != nil || len(token) != 43 || len(b) != 32 || base64.RawURLEncoding.EncodeToString(b) != token {
		return [32]byte{}, ErrSession
	}
	return sha256.Sum256(b), nil
}

func decodePHC(phc string) ([]byte, []byte, bool) {
	p := strings.Split(phc, "$")
	if len(p) != 6 || p[0] != "" || p[1] != "argon2id" || p[2] != "v=19" || p[3] != "m=19456,t=2,p=1" {
		return nil, nil, false
	}
	salt, e1 := base64.RawStdEncoding.Strict().DecodeString(p[4])
	hash, e2 := base64.RawStdEncoding.Strict().DecodeString(p[5])
	if e1 != nil || e2 != nil || len(salt) != 16 || len(hash) != 32 || base64.RawStdEncoding.EncodeToString(salt) != p[4] || base64.RawStdEncoding.EncodeToString(hash) != p[5] {
		return nil, nil, false
	}
	return salt, hash, true
}
func (s *Service) verifyPassword(password, phc string, eligible bool) bool {
	salt, expected, valid := decodePHC(phc)
	if !eligible || !valid {
		salt, expected, _ = decodePHC(dummyPHC)
	}
	actual := s.deriveKey([]byte(password), salt, 2, 19456, 1, 32)
	matched := subtle.ConstantTimeCompare(actual, expected) == 1
	return eligible && valid && matched
}

func (s *Service) mac(domain, payload string) string {
	h := hmac.New(sha256.New, s.cfg.SigningKey)
	h.Write([]byte("osc-login-v1\x00" + domain + "\x00" + s.cfg.Origin + "\x00" + payload))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func (s *Service) sign(domain, payload string) string { return payload + "." + s.mac(domain, payload) }
func (s *Service) verified(domain, signed string) (string, bool) {
	at := strings.LastIndexByte(signed, '.')
	if at < 1 || len(signed) > 160 {
		return "", false
	}
	payload, supplied := signed[:at], signed[at+1:]
	return payload, subtle.ConstantTimeCompare([]byte(s.mac(domain, payload)), []byte(supplied)) == 1
}
func (s *Service) SessionCSRF(token string) string {
	if _, e := tokenHash(token); e != nil {
		return ""
	}
	return s.mac("session-csrf", token)
}
func (s *Service) VerifySessionCSRF(token, supplied string) bool {
	expected := s.SessionCSRF(token)
	return expected != "" && subtle.ConstantTimeCompare([]byte(expected), []byte(supplied)) == 1
}
func (s *Service) NewChallenge() (string, error) {
	raw, e := newToken()
	if e != nil {
		return "", e
	}
	return s.sign("anonymous-challenge", raw+"."+strconv.FormatInt(s.cfg.Now().Add(5*time.Minute).Unix(), 10)), nil
}
func (s *Service) VerifyChallenge(token string) bool {
	payload, valid := s.verified("anonymous-challenge", token)
	if !valid {
		return false
	}
	p := strings.Split(payload, ".")
	if len(p) != 2 {
		return false
	}
	if _, e := tokenHash(p[0]); e != nil {
		return false
	}
	expires, e := strconv.ParseInt(p[1], 10, 64)
	now := s.cfg.Now()
	return e == nil && strconv.FormatInt(expires, 10) == p[1] && expires > now.Unix() && expires <= now.Add(5*time.Minute).Unix()
}
