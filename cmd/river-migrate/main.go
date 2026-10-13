// Controlled queue installer: separate River history, schema and DDL owner.
// Depends on pinned River/pgx; check_pg and operators call this before app
// migrations. API and worker never run migrations or receive the owner role.
package main

import (
	"context"
	"fmt"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"github.com/riverqueue/river/rivermigrate"
	"os"
	"time"
)

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	pool, err := pgxadapter.OpenMigrationPool(ctx, url)
	if err != nil {
		return fmt.Errorf("installer connection configuration failed")
	}
	defer pool.Close()
	verify := len(os.Args) == 2 && os.Args[1] == "--verify"
	if len(os.Args) > 1 && !verify {
		return fmt.Errorf("only --verify is supported")
	}
	if !verify {
		_, err = pool.Exec(ctx, `DO $prep$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_river_migrator') THEN
 CREATE ROLE app_river_migrator NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
 END IF;
 IF EXISTS(SELECT FROM pg_roles WHERE rolname='app_river_migrator' AND
 (rolcanlogin OR rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication OR rolinherit))
 OR EXISTS(SELECT FROM pg_auth_members WHERE member='app_river_migrator'::regrole)
 OR EXISTS(SELECT FROM pg_roles WHERE NOT rolcanlogin AND rolname<>'app_river_migrator'
 AND pg_has_role(oid,'app_river_migrator','MEMBER')) THEN
 RAISE EXCEPTION 'RIVER_MIGRATOR_UNSAFE'; END IF;
 IF NOT EXISTS(SELECT FROM pg_namespace WHERE nspname='queue') THEN
 CREATE SCHEMA queue AUTHORIZATION app_river_migrator; END IF;
 IF (SELECT nspowner FROM pg_namespace WHERE nspname='queue')<>'app_river_migrator'::regrole THEN
 RAISE EXCEPTION 'QUEUE_OWNER_UNSAFE'; END IF;
 REVOKE ALL ON SCHEMA queue FROM PUBLIC;
 END $prep$;`)
		if err != nil {
			return fmt.Errorf("controlled queue preparation failed: %w", err)
		}
	}
	// NOINHERIT does not prohibit SET ROLE. Reject outward or reverse/transitive
	// owner membership even in verify mode; only dedicated LOGIN members are allowed.
	_, err = pool.Exec(ctx, `DO $safe$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_river_migrator') THEN RAISE EXCEPTION 'RIVER_MIGRATOR_UNSAFE'; END IF;
 IF EXISTS(SELECT FROM pg_roles WHERE rolname='app_river_migrator' AND
 (rolcanlogin OR rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication OR rolinherit))
 OR EXISTS(SELECT FROM pg_auth_members WHERE member='app_river_migrator'::regrole)
 OR EXISTS(SELECT FROM pg_roles WHERE NOT rolcanlogin AND rolname<>'app_river_migrator'
 AND pg_has_role(oid,'app_river_migrator','MEMBER')) THEN RAISE EXCEPTION 'RIVER_MIGRATOR_UNSAFE'; END IF;
 IF (SELECT nspowner FROM pg_namespace WHERE nspname='queue') IS DISTINCT FROM 'app_river_migrator'::regrole THEN RAISE EXCEPTION 'QUEUE_OWNER_UNSAFE'; END IF;
 END $safe$;`)
	if err != nil {
		return fmt.Errorf("queue capability verification failed: %w", err)
	}
	owner, err := pgxadapter.OpenQueuePool(ctx, url, "app_river_migrator")
	if err != nil {
		return err
	}
	defer owner.Close()
	m, err := rivermigrate.New(owner.RiverDriver(), &rivermigrate.Config{Schema: "queue"})
	if err != nil {
		return err
	}
	if !verify {
		if _, err = m.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
			return err
		}
	}
	versions, err := m.ExistingVersions(ctx)
	if err != nil {
		return err
	}
	expected := m.AllVersions()
	if len(versions) != len(expected) {
		return fmt.Errorf("RIVER_HISTORY_INCOMPLETE")
	}
	for i, v := range versions {
		if v.Version != expected[i].Version {
			return fmt.Errorf("RIVER_HISTORY_UNKNOWN")
		}
	}
	fmt.Printf("PASS: River v0.47.0 queue history verified (%d migrations)\n", len(versions))
	return nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
// C-08d probe: identical-pixel input change.
