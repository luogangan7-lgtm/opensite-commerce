// Responsibilities: verify no-config composition remains safe and all optional
// runtime ports stay absent. Dependencies: bootstrap environment wiring; Go
// gates call this test, no real database/authentication is constructed.
package bootstrap_test

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"net/http/httptest"
	"testing"
)

func TestNoDatabaseRuntimeDefaultsDeny(t *testing.T) {
	t.Setenv("OPENSITE_DATABASE_URL", "")
	t.Setenv("OPENSITE_MEDIA_DATABASE_URL", "")
	t.Setenv("OPENSITE_MEDIA_ROOT", "")
	config, closeRuntime, err := bootstrap.FromEnvironment(context.Background(), bootstrap.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntime()
	if config.Catalog.Run != nil || config.Media.Run != nil || config.Fulfillment.Run != nil || config.Returns.Run != nil || config.Returns.Service != nil || config.Authenticator != nil {
		t.Fatal("unconfigured ports were fabricated")
	}
	server, err := bootstrap.New(config)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	server.Handler.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/t/11111111-1111-1111-1111-111111111111/products", nil))
	if rr.Code != 401 {
		t.Fatalf("default status=%d", rr.Code)
	}
}
