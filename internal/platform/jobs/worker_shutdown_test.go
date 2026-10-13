//go:build pgx

// Worker construction gate checks nil capabilities before deriving River drivers.
// Dependencies: job signer; callers: go test; invariant: nil pools return errors.
package jobs

import (
	"crypto/rand"
	"testing"
)

func TestNewWorkerRejectsNilPool(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	signer, err := NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Signer{nil, signer} {
		if client, err := NewWorker(nil, s, WorkerConfig{}); err == nil || client != nil {
			t.Fatal("nil pool must fail before driver construction")
		}
	}
}
