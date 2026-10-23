//go:build pgx

// Responsibilities: own disposable test logins, memberships and cancellation-safe cleanup.
// Dependencies: pgx and testing; called only by PostgreSQL test fixtures.
// Invariants: COMMON database lock precedes every role write; cleanup precedes unlock.
package pgxtest

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type roleSession struct {
	once   sync.Once
	conn   *pgx.Conn
	writer *pgx.Conn
}

var roleSessions sync.Map // *testing.T -> *roleSession; removed by test cleanup.

// FixtureRole never exposes the maintenance connection. One test may own multiple
// logins without recursively acquiring the common lock on different backends.
type FixtureRole struct {
	session      *roleSession
	name, target string
	dropped      bool
}

func roleSessionFor(t *testing.T, ctx context.Context) *roleSession {
	t.Helper()
	value, _ := roleSessions.LoadOrStore(t, &roleSession{})
	s := value.(*roleSession)
	s.once.Do(func() {
		// ponytail: serialize only fixtures that publish cluster memberships; remove
		// the lifetime lock only when migrations no longer reject live fixture logins.
		var err error
		s.conn, err = pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
		if err != nil {
			t.Fatal("role fixture common connection failed")
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if s.writer != nil {
				_ = s.writer.Close(cleanup)
			}
			// Closing the physical session releases the advisory lock even after errors.
			if err := s.conn.Close(cleanup); err != nil {
				t.Error("role fixture close failed", err)
			}
			roleSessions.Delete(t)
		})
		if _, err = s.conn.Exec(ctx, "SELECT pg_advisory_lock($1)", fixtureInitializationLockKey); err != nil {
			t.Fatal("role fixture common lock failed", err)
		}
		// Keep cancellation of role operations off the physical lock backend.
		s.writer, err = pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
		if err != nil {
			t.Fatal("role fixture writer connection failed")
		}
	})
	return s
}

// NewRole creates a uniquely named LOGIN and registers cleanup before returning.
// target is the database where this login can own grants/objects; DATABASE_URL is
// always the common lock database, including for fixtures in separate databases.
func NewRole(t *testing.T, ctx context.Context, target, name, password string, inherit bool, memberships ...string) *FixtureRole {
	t.Helper()
	s := roleSessionFor(t, ctx)
	r := &FixtureRole{session: s, name: name, target: target}
	t.Cleanup(func() { r.Drop(t, ctx) })
	inheritance := "NOINHERIT"
	if inherit {
		inheritance = "INHERIT"
	}
	var sql string
	if err := s.writer.QueryRow(ctx, "SELECT format('CREATE ROLE %I LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION %s PASSWORD %L', $1::text,$2::text,$3::text)", name, inheritance, password).Scan(&sql); err != nil {
		t.Fatal("role fixture credential quoting failed")
	}
	r.exec(t, ctx, sql)
	for _, membership := range memberships {
		r.Grant(t, ctx, membership, "")
	}
	return r
}

func (r *FixtureRole) exec(t *testing.T, ctx context.Context, sql string) {
	t.Helper()
	if _, err := r.session.writer.Exec(ctx, sql); err != nil {
		t.Fatal("role fixture write failed", err)
	}
}

// Grant changes only this fixture's membership; optional grantor is another fixture.
func (r *FixtureRole) Grant(t *testing.T, ctx context.Context, group, options string, grantor ...string) {
	t.Helper()
	switch options {
	case "", "SET FALSE", "ADMIN TRUE", "SET TRUE, ADMIN FALSE", "ADMIN OPTION":
	default:
		t.Fatal("unsupported role fixture membership options")
	}
	sql := "GRANT " + pgx.Identifier{group}.Sanitize() + " TO " + pgx.Identifier{r.name}.Sanitize()
	if options != "" {
		sql += " WITH " + options
	}
	if len(grantor) == 1 {
		sql += " GRANTED BY " + pgx.Identifier{grantor[0]}.Sanitize()
	}
	r.exec(t, ctx, sql)
}

// Revoke restores a negative membership probe before the next admission check.
func (r *FixtureRole) Revoke(t *testing.T, ctx context.Context, group string, grantor ...string) {
	t.Helper()
	sql := "REVOKE " + pgx.Identifier{group}.Sanitize() + " FROM " + pgx.Identifier{r.name}.Sanitize()
	if len(grantor) == 1 {
		sql += " GRANTED BY " + pgx.Identifier{grantor[0]}.Sanitize()
	}
	r.exec(t, ctx, sql)
}

// Alter permits only attributes used by admission probes, never arbitrary SQL.
func (r *FixtureRole) Alter(t *testing.T, ctx context.Context, attribute string) {
	t.Helper()
	for _, word := range strings.Fields(attribute) {
		switch word {
		case "BYPASSRLS", "NOBYPASSRLS", "INHERIT", "NOINHERIT", "CREATEDB", "NOCREATEDB", "NOLOGIN":
		default:
			t.Fatal("unsupported role fixture attribute")
		}
	}
	r.exec(t, ctx, "ALTER ROLE "+pgx.Identifier{r.name}.Sanitize()+" "+attribute)
}

// Drop ignores the caller's cancellation and has its own bounded cleanup budget.
// It is idempotent so tests can remove grantors before testing safe admission again.
func (r *FixtureRole) Drop(t *testing.T, ctx context.Context) {
	t.Helper()
	if r.dropped {
		return
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(cleanup, r.target)
	if err != nil {
		t.Error("role fixture cleanup connection failed")
		return
	}
	defer conn.Close(cleanup)
	var exists bool
	if err = conn.QueryRow(cleanup, "SELECT EXISTS(SELECT FROM pg_roles WHERE rolname=$1)", r.name).Scan(&exists); err != nil {
		t.Error("role fixture cleanup lookup failed", err)
		return
	}
	if exists {
		if _, err = conn.Exec(cleanup, "DROP OWNED BY "+pgx.Identifier{r.name}.Sanitize()+"; DROP ROLE "+pgx.Identifier{r.name}.Sanitize()); err != nil {
			t.Error("role fixture cleanup failed", err)
			return
		}
	}
	r.dropped = true
}
