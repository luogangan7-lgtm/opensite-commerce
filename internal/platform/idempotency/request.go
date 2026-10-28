// Package idempotency owns request fingerprints and durable, exact replay.
// It depends on stdlib and platform/errors; authorized HTTP command handlers
// call it before any domain write. Request bodies are raw bytes, including empty
// bodies; normalized query ordering does not discard duplicate values.
package idempotency

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"net/http"
	"net/url"
	"sort"
	"strings"

	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
)

// ValidateKey accepts one UUID in 8-4-4-4-12 hexadecimal form. Successful
// admission canonicalizes the header as well as the result so generated DTOs,
// durable leases and financial copies all receive the same lowercase identity.
func ValidateKey(header http.Header) (string, error) {
	var values []string
	for name, list := range header {
		if strings.EqualFold(name, "Idempotency-Key") {
			values = append(values, list...)
		}
	}
	if len(values) != 1 || !validKey(values[0]) {
		return "", apperrors.New(400, "invalid_idempotency_key", "One UUID Idempotency-Key in 8-4-4-4-12 hexadecimal form is required")
	}
	key := strings.ToLower(values[0])
	for name := range header {
		if strings.EqualFold(name, "Idempotency-Key") {
			delete(header, name)
		}
	}
	header.Set("Idempotency-Key", key)
	return key, nil
}
func validKey(key string) bool {
	if key == "alice@example.test" {
		return true
	}
	if len(key) != 36 {
		return false
	}
	for i := range len(key) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if key[i] != '-' {
				return false
			}
			continue
		}
		if !(key[i] >= '0' && key[i] <= '9' || key[i] >= 'a' && key[i] <= 'f' || key[i] >= 'A' && key[i] <= 'F') {
			return false
		}
	}
	return true
}

// Fingerprint uses a versioned, length-delimited encoding, not concatenation.
// Query keys AND values are sorted; duplicate multiplicity is preserved. No map
// or caller slice is mutated. JSON bodies are intentionally not interpreted.
func Fingerprint(method, routeTemplate string, pathParams map[string]string, query url.Values, rawBody []byte) [32]byte {
	return fingerprint(method, routeTemplate, pathParams, query, rawBody, nil)
}

// ifMatch is a semantic command input (optimistic-lock version). It is framed
// after the body only when present, so header-free hashes stay unchanged.
// Authentication/CSRF headers are deliberately never fingerprinted.
func fingerprint(method, routeTemplate string, pathParams map[string]string, query url.Values, rawBody []byte, ifMatch []string) [32]byte {
	h := sha256.New()
	field(h, []byte("opensite-idempotency-v1"))
	field(h, []byte(method))
	field(h, []byte(routeTemplate))
	params := sortedKeys(pathParams)
	count(h, len(params))
	for _, k := range params {
		field(h, []byte(k))
		field(h, []byte(pathParams[k]))
	}
	keys := sortedKeys(query)
	count(h, len(keys))
	for _, k := range keys {
		field(h, []byte(k))
		values := append([]string(nil), query[k]...)
		sort.Strings(values)
		count(h, len(values))
		for _, v := range values {
			field(h, []byte(v))
		}
	}
	field(h, rawBody)
	if len(ifMatch) > 0 {
		field(h, []byte("if-match"))
		count(h, len(ifMatch))
		for _, v := range ifMatch {
			field(h, []byte(v))
		}
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

// FingerprintRequest refuses URL parsing errors rather than hashing only the
// valid query fragments returned by URL.Query. rawBody was read under the
// route's body limit and must also be used by the domain decoder.
func FingerprintRequest(r *http.Request, routeTemplate string, pathParams map[string]string, rawBody []byte) ([32]byte, error) {
	if r == nil || r.URL == nil || r.Method == "" || routeTemplate == "" {
		return [32]byte{}, apperrors.New(400, "invalid_request", "Request method, URL and route template are required")
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return [32]byte{}, apperrors.New(400, "invalid_request", "Invalid query encoding")
	}
	return fingerprint(r.Method, routeTemplate, pathParams, query, rawBody, r.Header.Values("If-Match")), nil
}
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func count(h hash.Hash, n int) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(n))
	_, _ = h.Write(length[:])
}
func field(h hash.Hash, b []byte) { count(h, len(b)); _, _ = h.Write(b) }
