// Tests exercise actual pgx parse/connect failure boundaries, never SQL behavior.
// Called by Go tests; credentials and driver errors must remain private.
package store

import (
	"context"
	"strings"
	"testing"
)

func TestParseAndConnectFailuresAreRedacted(t *testing.T) {
	secret := "sentinel-secret-PHC-password"
	for _, dsn := range []string{"postgres://user:" + secret + "@[invalid", "postgres://user:" + secret + "@127.0.0.1:1/missing?sslmode=disable"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		id, err := Bootstrap(ctx, dsn, "owner@example.com", "Store", secret)
		if id != "" || err == nil || err.Error() != "bootstrap database operation failed" || strings.Contains(err.Error(), secret) {
			t.Fatal("database error escaped boundary")
		}
	}
}
