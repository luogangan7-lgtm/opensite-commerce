#!/usr/bin/env python3
"""Local migrator: cluster preflight, immutable migrations, least-privilege logins.

Depends on PG18 psql/bash and environment secrets; compose migration job calls it.
No credentials enter arguments/output; uninspectable databases fail closed.
Reused logins have no ownership/policy targets or direct grants except target CONNECT.
"""
import argparse
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
# Role names only (no credentials): login role per component, and the NOLOGIN
# capability group it is granted. Passwords come from env/stdin, never source.
LOGIN_NAMES = {'RUNTIME': 'opensite_runtime_login', 'AUTH': 'opensite_auth_login',
               'DOMAIN': 'opensite_domain_login', 'MEDIA': 'opensite_media_login',
               'WORKER': 'opensite_queue_login', 'OUTBOX': 'opensite_outbox_login',
               'CUSTOMER_AUTH': 'opensite_customer_auth_login', 'RETURNS': 'opensite_returns_login'}
CAPABILITY_GROUPS = {'RUNTIME': 'app_runtime', 'AUTH': 'app_auth',
                     'DOMAIN': 'app_domain_resolver', 'MEDIA': 'app_media_service',
                     'WORKER': 'app_queue_worker', 'OUTBOX': 'app_outbox_worker',
                     'CUSTOMER_AUTH': 'app_customer_auth', 'RETURNS': 'app_returns_service'}
LOGIN_ROLES = {kind: (LOGIN_NAMES[kind], CAPABILITY_GROUPS[kind]) for kind in LOGIN_NAMES}
SYSTEM_DATABASES = {'postgres', 'template0', 'template1'}

class DeployError(RuntimeError): pass

def database_name(value):
    if not re.fullmatch(r'[a-z][a-z0-9_]{0,62}', value):
        raise DeployError('INVALID_DATABASE_NAME')
    return value

def target_database_name(value):
    # Maintenance/template DBs may be inspected, never migrated or provisioned.
    if database_name(value) in SYSTEM_DATABASES:
        raise DeployError('SYSTEM_DATABASE_TARGET')
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
    target_database_name(target)
    # A disabled application database still shares roles with this cluster.
    # Attempt every non-template DB; disabled/uninspectable targets fail closed.
    databases = fetch("SELECT datname FROM pg_database WHERE NOT datistemplate ORDER BY datname;", 'postgres').splitlines()
    for database in databases:
        database_name(database)  # conservative: unsupported names cannot be silently skipped
        flags = fetch("SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='app'), to_regclass('public.schema_migrations') IS NOT NULL;", database).strip()
        if flags not in ('t|t', 't|f', 'f|t', 'f|f'):
            raise DeployError('DATABASE_INSPECTION_FAILED')
        if database != target and flags == 't|t':
            raise DeployError('SECOND_APPLICATION_DATABASE')

def login_role_audit_sql(database):
    database_name(database)
    logins = ', '.join(literal(login) for login, _ in LOGIN_ROLES.values())
    # Shared dependencies cover ownership, ACLs (including columns), and policy
    # targets in every database. PG18: o=owner, a=ACL, i=initial ACL, r=RLS policy.
    # Only our prior nongrantable CONNECT may survive a normal provisioning rerun.
    return f"""WITH logins AS (
 SELECT oid FROM pg_catalog.pg_roles WHERE rolname IN ({logins})
)
SELECT EXISTS (
 SELECT 1 FROM pg_catalog.pg_shdepend d JOIN logins l ON l.oid=d.refobjid
 WHERE d.refclassid='pg_catalog.pg_authid'::regclass
 AND d.deptype IN ('o', 'a', 'i', 'r')
 AND NOT (d.deptype='a' AND d.classid='pg_catalog.pg_database'::regclass
          AND d.dbid=0 AND d.objsubid=0
          AND d.objid IN (SELECT oid FROM pg_catalog.pg_database WHERE datname={literal(database)}))
) OR EXISTS (
 SELECT 1 FROM pg_catalog.pg_database d
 CROSS JOIN LATERAL pg_catalog.aclexplode(d.datacl) a
 JOIN logins l ON a.grantee=l.oid OR a.grantor=l.oid
 WHERE d.datname={literal(database)}
 AND NOT (a.grantee=l.oid AND a.grantor<>l.oid
          AND a.privilege_type='CONNECT' AND NOT a.is_grantable)
)"""

def audit_login_roles(database, fetch=query):
    result = fetch(login_role_audit_sql(database) + ';', 'postgres').strip()
    if result == 't':
        raise DeployError('TAINTED_LOGIN_ROLE: direct privileges, object ownership or policy targets require administrator review')
    if result != 'f':
        raise DeployError('DATABASE_INSPECTION_FAILED')

def provision_sql(database, passwords):
    target_database_name(database)
    verified = {kind: secret(passwords.get(kind, '')) for kind in LOGIN_ROLES}
    if len(set(verified.values())) != len(verified) or os.environ.get('POSTGRES_PASSWORD', '') in verified.values():
        raise DeployError('DUPLICATE_LOGIN_PASSWORD')
    lines = ["SET standard_conforming_strings=on;", "SET log_statement='none';",
             "SET log_min_error_statement='panic';", "SET log_parameter_max_length_on_error=0;", 'BEGIN;',
             f"DO $login_audit$ BEGIN IF ({login_role_audit_sql(database)}) THEN RAISE EXCEPTION 'TAINTED_LOGIN_ROLE: direct privileges, object ownership or policy targets require administrator review'; END IF; END $login_audit$;",
             f'REVOKE CONNECT ON DATABASE "{database}" FROM PUBLIC;']
    for kind, (login, group) in LOGIN_ROLES.items():
        password = verified[kind]
        lines.append(f"DO $role$ BEGIN IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname={literal(login)}) THEN CREATE ROLE {login}; END IF; END $role$;")
        lines.append(f"ALTER ROLE {login} LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD {literal(password)};")
        # Rotation/re-run cannot preserve accidental capability memberships.
        lines.append(f"DO $membership$ DECLARE r record; BEGIN FOR r IN SELECT roleid::regrole::text AS name FROM pg_auth_members WHERE member=(SELECT oid FROM pg_roles WHERE rolname={literal(login)}) LOOP EXECUTE format('REVOKE %I FROM {login}',r.name); END LOOP; END $membership$;")
        lines.extend([f'GRANT {group} TO {login};', f'GRANT CONNECT ON DATABASE "{database}" TO {login};'])
        if kind == 'RETURNS':
            lines.append('GRANT app_runtime TO opensite_returns_login;')
    lines.append('COMMIT;')
    return '\n'.join(lines)

def install_migrations(env):
    # River owns its separate history: schema/install, app grants, history verify.
    for command in (['/usr/local/bin/river-migrate'],
                    ['bash', str(ROOT / 'scripts/migrate.sh')],
                    ['/usr/local/bin/river-migrate', '--verify']):
        result = subprocess.run(command, env=env, cwd=ROOT, capture_output=True, text=True, timeout=300)
        if result.returncode == 7 and command[-1].endswith('migrate.sh'):
            raise DeployError('MIGRATION_REFUSED_PENDING: migrations/pending/ is not empty; land them with scripts/land_migration.sh before deploying')
        if result.returncode:
            raise DeployError('MIGRATION_FAILED: inspect source/checksum history with the migration gate')

def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['preflight', 'install', 'provision', 'verify'])
    args = parser.parse_args(argv)
    try:
        database = target_database_name(os.environ.get('OPENSITE_DB', 'opensite'))
        secret(os.environ.get('POSTGRES_PASSWORD', ''))
        preflight(database)
        print('PASS: cluster preflight (one OpenSite application database)')
        if args.action == 'preflight': return 0
        env = dict(os.environ, PGPASSWORD=os.environ['POSTGRES_PASSWORD'],
                   DATABASE_URL=f"host={os.environ.get('OPENSITE_DB_HOST', 'postgres')} dbname={database} user=postgres connect_timeout=5")
        if args.action == 'verify':
            result = subprocess.run(['/usr/local/bin/river-migrate','--verify'], env=env, cwd=ROOT, capture_output=True, timeout=180)
            if result.returncode: raise DeployError('RIVER_HISTORY_VERIFICATION_FAILED')
            print('PASS: restored River history matches locked installer')
            return 0
        passwords = {kind: os.environ.get(f'OPENSITE_{kind}_DB_PASSWORD', '') for kind in LOGIN_ROLES}
        provisioning = provision_sql(database, passwords)  # validate before any migration writes
        audit_login_roles(database)
        if args.action == 'install':
            install_migrations(env)
            print('PASS: River install → forward migrations/checksums → River history verified')
        query(provisioning, database)
        print('PASS: separate restricted login roles; PUBLIC CONNECT revoked')
        return 0
    except (DeployError, subprocess.TimeoutExpired) as exc:
        print(str(exc) if isinstance(exc, DeployError) else 'MIGRATION_TIMEOUT', file=sys.stderr)
        return 1

if __name__ == '__main__': sys.exit(main())
