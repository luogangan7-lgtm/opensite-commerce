// Deployment URL tests guard secret encoding and fail-closed component config.
// Dependencies: environmentDSN and net/url; Go gates execute without a database.
package bootstrap

import (
	"bytes"
	"context"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestEnvironmentDSN(t *testing.T) {
	t.Setenv("OPENSITE_DATABASE_URL", "")
	t.Setenv("OPENSITE_DB_HOST", "postgres")
	t.Setenv("OPENSITE_DB", "opensite")
	t.Setenv("OPENSITE_RUNTIME_DB_PASSWORD", "test_only_special@': /?&=value")
	got, err := environmentDSN("OPENSITE_DATABASE_URL", "RUNTIME", "opensite_runtime_login")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := parsed.User.Password()
	if password != "test_only_special@': /?&=value" || parsed.User.Username() != "opensite_runtime_login" || parsed.Host != "postgres:5432" {
		t.Fatal("credential components changed")
	}
	t.Setenv("OPENSITE_DB_HOST", "bad/host")
	_, err = environmentDSN("OPENSITE_DATABASE_URL", "RUNTIME", "opensite_runtime_login")
	if err == nil || strings.Contains(err.Error(), password) {
		t.Fatal("invalid host accepted or secret leaked")
	}
}

func TestReturnsBindingMissingLogsSettingWithoutEnablingRefund(t *testing.T) {
	for _, key := range []string{"OPENSITE_RETURNS_DATABASE_URL", "OPENSITE_RETURNS_DB_PASSWORD"} {
		old, present := os.LookupEnv(key)
		os.Unsetenv(key)
		t.Cleanup(func() {
			if present {
				os.Setenv(key, old)
			} else {
				os.Unsetenv(key)
			}
		})
	}
	var output bytes.Buffer
	config, closeRuntime, err := FromEnvironment(context.Background(), Config{Logger: slog.New(slog.NewTextHandler(&output, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntime()
	if config.Returns.Service != nil || config.ReturnsCoordinator.Runner != nil {
		t.Fatal("RETURNS_BINDING_RED: credential alone enabled refund")
	}
	if !strings.Contains(output.String(), "OPENSITE_RETURNS_DATABASE_URL") {
		t.Fatal("RETURNS_BINDING_RED: missing binding was silent")
	}
}

func TestReturnsEnvironmentDSN(t *testing.T) {
	t.Setenv("OPENSITE_RETURNS_DATABASE_URL", "")
	t.Setenv("OPENSITE_DB_HOST", "postgres")
	t.Setenv("OPENSITE_DB", "opensite")
	secret := "test_only_returns@': /?&=value"
	t.Setenv("OPENSITE_RETURNS_DB_PASSWORD", secret)
	got, err := environmentDSN("OPENSITE_RETURNS_DATABASE_URL", "RETURNS", "opensite_returns_login")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := parsed.User.Password()
	if password != secret || parsed.User.Username() != "opensite_returns_login" {
		t.Fatal("returns components changed")
	}
	t.Setenv("OPENSITE_RETURNS_DB_PASSWORD", "short")
	if _, err = environmentDSN("OPENSITE_RETURNS_DATABASE_URL", "RETURNS", "opensite_returns_login"); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("invalid credential accepted or leaked")
	}
}
