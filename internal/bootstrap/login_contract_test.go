// Responsibilities: verify full bootstrap registration with real identity transport.
// Dependencies: bootstrap, identity testfixture and the shared strict reconciler.
// Caller: Go gates; invariants: complete OpenAPI, no database/secrets or handler IO.
package bootstrap_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/testfixture"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func TestConfiguredIdentityRouteContract(t *testing.T) {
	// Construct the production transport without a repository. Route registration
	// needs no database; this test inspects registrations without invoking handlers.
	handler := testfixture.LoginHandler("https://admin.example.test", 30*time.Minute)
	server, err := bootstrap.New(bootstrap.Config{Routes: handler.Routes(), Authenticator: handler})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, ok := server.Handler.(interface{ RouteSpecs() []httpapi.RouteSpec })
	if !ok {
		t.Fatal("bootstrap must expose successful registration snapshots")
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileRoutes(doc, snapshot.RouteSpecs()); err != nil {
		t.Fatal(err)
	}
	// Removing any installed business handler must fail the full merged contract.
	for i, route := range snapshot.RouteSpecs() {
		if route.OperationID == "health" {
			continue
		}
		t.Run("missing_"+route.OperationID, func(t *testing.T) {
			specs := snapshot.RouteSpecs()
			specs = append(specs[:i:i], specs[i+1:]...)
			if err := reconcileRoutes(doc, specs); err == nil {
				t.Fatal("missing registered handler accepted")
			}
		})
	}
}

func TestLoginFixtureExcludedFromProductionDependencies(t *testing.T) {
	for _, mode := range []struct {
		name string
		args []string
	}{
		{"default", []string{"list", "-deps", "./cmd/api"}},
		{"pgx", []string{"list", "-deps", "-tags", "pgx", "./cmd/api"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "go", mode.args...)
			cmd.Dir = "../.."
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("production dependency lookup failed: %v", err)
			}
			for _, path := range strings.Fields(string(output)) {
				if strings.Contains(path, "/identity/testfixture") {
					t.Fatalf("test fixture entered production dependency graph: %s", path)
				}
			}
		})
	}
}
