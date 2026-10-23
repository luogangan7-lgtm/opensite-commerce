//go:build pgx

// Responsibility: isolate committed ACL fault probes from parallel pgx packages.
// Dependencies: real migrations, psql and the common pgxtest initialization lock.
// Called by customer privilege-drift tests; normal startup must remain available.
package customer_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

// Never put connection credentials in process arguments or installer diagnostics.
func customerPrivilegePSQL(ctx context.Context, dsn, sql string, args ...string) error {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil || u.Hostname() == "" || u.Path == "" {
		return errors.New("privilege fixture URL invalid")
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	secret, _ := u.User.Password()
	env := make([]string, 0, len(os.Environ())+8)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !strings.HasPrefix(key, "PG") && key != "DATABASE_URL" {
			env = append(env, item)
		}
	}
	fields := map[string]string{"PGHOST": u.Hostname(), "PGPORT": port, "PGUSER": u.User.Username(), "PGPASSWORD": secret, "PGDATABASE": strings.TrimPrefix(u.Path, "/")}
	for name, key := range map[string]string{"sslmode": "PGSSLMODE", "connect_timeout": "PGCONNECT_TIMEOUT", "sslrootcert": "PGSSLROOTCERT", "sslcert": "PGSSLCERT", "sslkey": "PGSSLKEY", "options": "PGOPTIONS"} {
		if value := u.Query().Get(name); value != "" {
			fields[key] = value
		}
	}
	for key, value := range fields {
		env = append(env, key+"="+value)
	}
	cmd := exec.CommandContext(ctx, "psql", append([]string{"-X", "-q", "-v", "ON_ERROR_STOP=1"}, args...)...)
	cmd.Env, cmd.Stdin = env, strings.NewReader(sql)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run()
}

func customerPrivilegeDSNs(t *testing.T) (string, string) {
	t.Helper()
	source, auth := os.Getenv("DATABASE_URL"), os.Getenv("CUSTOMER_AUTH_TEST_DATABASE_URL")
	if source == "" || auth == "" {
		pgxtest.Skip(t, "NOT_RUN: disposable owner and customer PostgreSQL DSNs required")
	}
	ownerURL, err := url.Parse(source)
	if err != nil || ownerURL.User == nil || ownerURL.Hostname() == "" {
		t.Fatal("privilege fixture owner URL invalid")
	}
	authURL, err := url.Parse(auth)
	if err != nil || authURL.User == nil || authURL.Hostname() == "" {
		t.Fatal("privilege fixture customer URL invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	database := "osc_customer_privilege_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if customerPrivilegePSQL(ctx, source, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()+";") != nil {
		t.Fatal("privilege fixture database creation failed")
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if customerPrivilegePSQL(cleanup, source, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE);") != nil {
			t.Error("privilege fixture database cleanup failed")
		}
	})
	ownerURL.Path, authURL.Path = "/"+database, "/"+database
	ownerDSN := ownerURL.String()
	migrations, err := filepath.Glob("../../../migrations/[0-9][0-9][0-9]_*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("privilege fixture migrations unavailable")
	}
	maintenance, err := pgxadapter.OpenIdentity(ctx, source)
	if err != nil {
		t.Fatal("privilege fixture maintenance connection unavailable")
	}
	defer maintenance.Close(context.Background())
	if err := pgxtest.WithFixtureInitialization(ctx, maintenance.InMaintenance, func(ctx context.Context) error {
		// Glob is sorted; CI normalizes all pending migrations before Go runs.
		// The common source lock serializes the cluster role DDL in migrations.
		for _, path := range migrations {
			if strings.HasSuffix(path, "_queue_authorization.sql") {
				installer := exec.CommandContext(ctx, "bash", "../../../scripts/migrate_river.sh")
				installer.Env = append(os.Environ(), "DATABASE_URL="+ownerDSN)
				installer.Stdout, installer.Stderr = io.Discard, io.Discard
				if installer.Run() != nil {
					return errors.New("privilege fixture River installation failed")
				}
			}
			if customerPrivilegePSQL(ctx, ownerDSN, "", "-1", "-f", path) != nil {
				return fmt.Errorf("privilege fixture migration failed: %s", filepath.Base(path))
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return ownerDSN, authURL.String()
}
