#!/usr/bin/env python3
"""Local migrator: cluster preflight, immutable migrations, least-privilege logins.

Depends on PG18 psql/bash and environment secrets; compose migration job calls it.
No credentials enter arguments/output; uninspectable databases fail closed.
"""
import argparse
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
LOGIN_ROLES = {'RUNTIME': ('opensite_runtime_login', 'app_runtime'),
               'AUTH': ('opensite_auth_login', 'app_auth'),
               'DOMAIN': ('opensite_domain_login', 'app_domain_resolver'),
               'MEDIA': ('opensite_media_login', 'app_media_service')}

class DeployError(RuntimeError): pass

def database_name(value):
    if not re.fullmatch(r'[a-z][a-z0-9_]{0,62}', value):
        raise DeployError('INVALID_DATABASE_NAME')
    return value

def secret(value):
    if len(value) < 24 or re.search(r'[\x00-\x1f\x7f]', value) or value.lower().startswith('replace'):
        raise DeployError('INVALID_ENV_SECRET: use a random single-line value of at least 24 characters')
    return value

def literal(value):
    return "'" + value.replace("'", "''") + "'"

def query(sql, database):
    database_name(database)
    env = dict(os.environ, PGPASSWORD=os.environ.get('POSTGRES_PASSWORD', ''),
               PGHOST=os.environ.get('OPENSITE_DB_HOST', 'postgres'), PGPORT='5432',
               PGUSER='postgres', PGDATABASE=database, PGCONNECT_TIMEOUT='5')
    try:
        result = subprocess.run(['psql', '-X', '-qAt', '-v', 'ON_ERROR_STOP=1'],
                                input=sql, text=True, capture_output=True, env=env, timeout=60)
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise DeployError('DATABASE_INSPECTION_FAILED') from exc
    if result.returncode:
        # SQL may contain passwords; never print stderr/statement/connection info.
        raise DeployError('DATABASE_OPERATION_FAILED')
    return result.stdout

def preflight(target, fetch=query):
    database_name(target)
    databases = fetch("SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname;", 'postgres').splitlines()
    for database in databases:
        database_name(database)  # conservative: unsupported names cannot be silently skipped
        flags = fetch("SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='app'), to_regclass('public.schema_migrations') IS NOT NULL;", database).strip()
        if flags not in ('t|t', 't|f', 'f|t', 'f|f'):
            raise DeployError('DATABASE_INSPECTION_FAILED')
        if database != target and flags == 't|t':
            raise DeployError('SECOND_APPLICATION_DATABASE')

def provision_sql(database, passwords):
    database_name(database)
    lines = ["SET standard_conforming_strings=on;", "SET log_statement='none';",
             "SET log_min_error_statement='panic';", "SET log_parameter_max_length_on_error=0;", 'BEGIN;',
             f'REVOKE CONNECT ON DATABASE "{database}" FROM PUBLIC;']
    for kind, (login, group) in LOGIN_ROLES.items():
        password = secret(passwords.get(kind, ''))
        lines.append(f"DO $role$ BEGIN IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname={literal(login)}) THEN CREATE ROLE {login}; END IF; END $role$;")
        lines.append(f"ALTER ROLE {login} LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD {literal(password)};")
        # Rotation/re-run cannot preserve accidental capability memberships.
        lines.append(f"DO $membership$ DECLARE r record; BEGIN FOR r IN SELECT roleid::regrole::text AS name FROM pg_auth_members WHERE member=(SELECT oid FROM pg_roles WHERE rolname={literal(login)}) LOOP EXECUTE format('REVOKE %I FROM {login}',r.name); END LOOP; END $membership$;")
        lines.extend([f'GRANT {group} TO {login};', f'GRANT CONNECT ON DATABASE "{database}" TO {login};'])
    lines.append('COMMIT;')
    return '\n'.join(lines)

def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['preflight', 'install', 'provision'])
    args = parser.parse_args(argv)
    try:
        database = database_name(os.environ.get('OPENSITE_DB', 'opensite'))
        secret(os.environ.get('POSTGRES_PASSWORD', ''))
        preflight(database)
        print('PASS: cluster preflight (one OpenSite application database)')
        if args.action == 'preflight': return 0
        passwords = {kind: os.environ.get(f'OPENSITE_{kind}_DB_PASSWORD', '') for kind in LOGIN_ROLES}
        provisioning = provision_sql(database, passwords)  # validate before any migration writes
        if args.action == 'install':
            env = dict(os.environ, PGPASSWORD=os.environ['POSTGRES_PASSWORD'],
                       DATABASE_URL=f"host={os.environ.get('OPENSITE_DB_HOST', 'postgres')} dbname={database} user=postgres connect_timeout=5")
            result = subprocess.run(['bash', str(ROOT / 'scripts/migrate.sh')], env=env, cwd=ROOT, capture_output=True, text=True, timeout=300)
            if result.returncode: raise DeployError('MIGRATION_FAILED: inspect source/checksum history with the migration gate')
            print('PASS: forward migrations and checksums verified')
        query(provisioning, database)
        print('PASS: separate restricted login roles; PUBLIC CONNECT revoked')
        return 0
    except (DeployError, subprocess.TimeoutExpired) as exc:
        print(str(exc) if isinstance(exc, DeployError) else 'MIGRATION_TIMEOUT', file=sys.stderr)
        return 1

if __name__ == '__main__': sys.exit(main())
