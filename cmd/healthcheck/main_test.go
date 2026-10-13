// Tests exercise HTTP status, redirect and timeout gates against local servers.
// They call the probe directly; no real network or database is required.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeStatusAndRedirect(t *testing.T) {
	for _, status := range []int{204, 200, 302, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "/healthz")
				w.WriteHeader(status)
			}))
			defer server.Close()
			err := check(context.Background(), server.URL)
			if (err == nil) != (status == 204) {
				t.Fatalf("status %d: %v", status, err)
			}
		})
	}
}

func TestProbeTimeoutAndInvalidURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if check(ctx, server.URL) == nil {
		t.Fatal("timeout succeeded")
	}
	for _, address := range []string{"file:///etc/passwd", "http://user:secret@example.com/healthz", "http://example.com/healthz?token=secret", "http://example.com/healthz#secret", ":invalid"} {
		if check(context.Background(), address) == nil {
			t.Errorf("accepted invalid URL %q", address)
		}
	}
}
