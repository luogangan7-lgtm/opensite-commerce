package pgxadapter

import (
	"context"
	"testing"
)

func TestQueuePoolRejectsUnapprovedRoleBeforeConnection(t *testing.T) {
	for _, role := range []string{"", "postgres", "app_runtime", "app_auth", "app_queue_worker; RESET ROLE"} {
		pool, err := OpenQueuePool(context.Background(), "invalid DSN", role)
		if pool != nil || err == nil || err.Error() != "unsupported queue connection role" {
			t.Fatalf("role %q admitted or wrong error: %v", role, err)
		}
	}
}

func TestQueuePoolApprovedRolesHaveConnectGuard(t *testing.T) {
	for _, role := range []string{"app_queue_worker", "app_outbox_worker", "app_river_migrator"} {
		cfg, err := queuePoolConfig("postgresql://localhost/unused", role)
		if err != nil || cfg == nil || cfg.AfterConnect == nil {
			t.Fatalf("missing role guard for %s: %v", role, err)
		}
	}
}
