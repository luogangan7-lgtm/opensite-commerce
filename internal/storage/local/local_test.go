package local

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestPutOpenAndDedup(t *testing.T) {
	s, e := New(t.TempDir(), 1024)
	if e != nil {
		t.Fatal(e)
	}
	src := []byte("image bytes for the local store")
	a, e := s.Put(context.Background(), bytes.NewReader(src))
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.Put(context.Background(), bytes.NewReader(src))
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(src)
	if a.SHA256 != hex.EncodeToString(h[:]) || a.Key != b.Key {
		t.Fatal("hash mismatch or failed dedup")
	}
	f, e := s.Open(a.Key)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	data, e := io.ReadAll(f)
	if e != nil || !bytes.Equal(src, data) {
		t.Fatal("blob altered", e)
	}
}
func TestOversize(t *testing.T) {
	s, _ := New(t.TempDir(), 4)
	if _, e := s.Put(context.Background(), strings.NewReader("12345")); e == nil {
		t.Fatal("oversize accepted")
	}
}
func TestTraversal(t *testing.T) {
	s, _ := New(t.TempDir(), 4)
	if _, e := s.Open("../../etc/passwd"); e == nil {
		t.Fatal("traversal accepted")
	}
}
func TestCancelled(t *testing.T) {
	s, _ := New(t.TempDir(), 4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := s.Put(ctx, strings.NewReader("x")); !errors.Is(e, context.Canceled) {
		t.Fatalf("want cancellation, got %v", e)
	}
}

func TestMalformedKeyDoesNotPanic(t *testing.T) {
	s, _ := New(t.TempDir(), 1024)
	key := strings.Repeat("a", 64) + "/b/c"
	if _, err := s.Open(key); err == nil {
		t.Fatal("invalid blob key accepted")
	}
}
