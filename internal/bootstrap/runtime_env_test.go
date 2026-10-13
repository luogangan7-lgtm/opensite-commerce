// Deployment URL tests guard secret encoding and fail-closed component config.
// Dependencies: environmentDSN and net/url; Go gates execute without a database.
package bootstrap

import (
	"net/url"
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
