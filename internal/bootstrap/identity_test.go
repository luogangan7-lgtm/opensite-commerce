// Composition tests keep missing/partial credentials fail-closed before DB IO.
// Dependencies: only bootstrap; production auth authority is tested with real PG.
package bootstrap

import (
	"context"
	"strings"
	"testing"
)

func TestStaffLoginConfiguration(t *testing.T) {
	m, e := StaffLogin(context.Background(), "", "", "")
	if e != nil || m != nil {
		t.Fatal("optional staff module incorrectly enabled")
	}
	for _, tc := range []struct{ dsn, origin, key string }{{"secret-dsn", "https://admin.example.test", ""}, {"", "https://admin.example.test", strings.Repeat("1", 64)}, {"secret-dsn", "", strings.Repeat("1", 64)}, {"secret-dsn", "https://admin.example.test", "not-hex"}, {"secret-dsn", "https://admin.example.test", strings.Repeat("1", 32)}} {
		m, e = StaffLogin(context.Background(), tc.dsn, tc.origin, tc.key)
		if e == nil || m != nil || strings.Contains(e.Error(), "secret-dsn") {
			t.Fatal("unsafe partial configuration")
		}
	}
}
