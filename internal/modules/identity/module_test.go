// Tests call the published identity port; invalid input cannot reach a database.
// Dependencies: public port; database and PG authorization gates are separate.
package identity_test

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity"
	"testing"
)

func TestPublicBootstrapPortFailsClosed(t *testing.T) {
	if id, err := identity.BootstrapAdmin(context.Background(), identity.BootstrapRequest{}); id != "" || err == nil || err.Error() != "bootstrap failed" {
		t.Fatal("public port accepted missing fields")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if id, err := identity.BootstrapAdmin(ctx, identity.BootstrapRequest{DatabaseURL: "dsn", Email: "owner@example.com", TenantName: "Store", Password: "long-passphrase"}); id != "" || err == nil {
		t.Fatal("public port ignored cancellation")
	}
}
