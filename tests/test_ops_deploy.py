"""Ops safety regressions: fake catalog for deterministic unit tests; real PG in ops_drill."""
import importlib.util
from pathlib import Path
import unittest
import os
import io
import re
import secrets
from unittest.mock import patch
from types import SimpleNamespace

spec = importlib.util.spec_from_file_location('ops_deploy', Path(__file__).resolve().parents[1] / 'scripts/ops_deploy.py')
ops = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ops)

class DeployTests(unittest.TestCase):
    def valid_passwords(self):
        return {kind: f'nonsecret_fixture_{kind}_123456789' for kind in ops.LOGIN_ROLES}

    def test_returns_login_has_only_trusted_coordinator_capability(self):
        self.assertEqual(ops.LOGIN_ROLES.get('RETURNS'),
                         ('opensite_returns_login', 'app_returns_service'),
                         'RETURNS_LOGIN_RED: dedicated refund coordinator is missing')
        sql = ops.provision_sql('opensite', {kind: secrets.token_urlsafe(32) for kind in ops.LOGIN_ROLES})
        self.assertIn('GRANT app_returns_service TO opensite_returns_login;', sql)
        self.assertNotIn('GRANT app_returns_service TO opensite_runtime_login;', sql)
        self.assertIn("'opensite_returns_login'", ops.login_role_audit_sql('opensite'))

    def test_customer_auth_login_has_only_its_capability(self):
        self.assertIn('CUSTOMER_AUTH', ops.LOGIN_ROLES,
                      'CUSTOMER_AUTH_DEPLOYMENT_RED: customer credential pool is not provisioned')
        self.assertEqual(ops.LOGIN_NAMES['CUSTOMER_AUTH'], 'opensite_customer_auth_login')
        self.assertEqual(ops.CAPABILITY_GROUPS['CUSTOMER_AUTH'], 'app_customer_auth')
        sql = ops.provision_sql('opensite', {kind: secrets.token_urlsafe(32) for kind in ops.LOGIN_ROLES})
        self.assertIn('GRANT app_customer_auth TO opensite_customer_auth_login;', sql)
        self.assertNotIn('GRANT app_customer_auth TO opensite_runtime_login;', sql)
        self.assertNotIn('GRANT app_auth TO opensite_customer_auth_login;', sql)
        self.assertIn("'opensite_customer_auth_login'", ops.login_role_audit_sql('opensite'))

    def test_missing_customer_auth_secret_refuses_before_writers(self):
        env = {f'OPENSITE_{kind}_DB_PASSWORD': secrets.token_urlsafe(32) for kind in ops.LOGIN_ROLES}
        env['POSTGRES_PASSWORD'] = secrets.token_urlsafe(32)
        env['OPENSITE_CUSTOMER_AUTH_DB_PASSWORD'] = str()
        with patch.dict(os.environ, env, clear=True), patch.object(ops, 'preflight'), \
                patch.object(ops, 'audit_login_roles') as audit, \
                patch.object(ops, 'install_migrations') as install, patch.object(ops, 'query') as query, \
                patch('sys.stdout', io.StringIO()), patch('sys.stderr', io.StringIO()) as error:
            self.assertEqual(ops.main(['install']), 1,
                             'CUSTOMER_AUTH_SECRET_RED: absent secret silently deploys without customer auth')
            self.assertIn('INVALID_ENV_SECRET', error.getvalue())
            audit.assert_not_called()
            install.assert_not_called()
            query.assert_not_called()

    def test_river_install_precedes_application_and_verify_follows(self):
        calls = []
        with patch.object(ops.subprocess, 'run', side_effect=lambda argv, **kw: calls.append(argv) or SimpleNamespace(returncode=0)):
            ops.install_migrations({'DATABASE_URL':'fixture'})
        self.assertEqual(calls[0], ['/usr/local/bin/river-migrate'])
        self.assertEqual(calls[1], ['bash', str(ops.ROOT / 'scripts/migrate.sh')])
        self.assertEqual(calls[2], ['/usr/local/bin/river-migrate', '--verify'])

    def test_river_failure_stops_before_application_migrations(self):
        calls = []
        with patch.object(ops.subprocess, 'run', side_effect=lambda argv, **kw: calls.append(argv) or SimpleNamespace(returncode=1)):
            with self.assertRaises(ops.DeployError): ops.install_migrations({'DATABASE_URL':'fixture'})
        self.assertEqual(calls, [['/usr/local/bin/river-migrate']])

    def test_pending_migration_refusal_is_reported_as_such(self):
        # migrate.sh exits 7 when migrations/pending/ is non-empty (fail closed in production).
        results = iter([0, 7])
        with patch.object(ops.subprocess, 'run', side_effect=lambda argv, **kw: SimpleNamespace(returncode=next(results))):
            with self.assertRaisesRegex(ops.DeployError, 'MIGRATION_REFUSED_PENDING'):
                ops.install_migrations({'DATABASE_URL': 'fixture'})

    def test_preflight_checks_every_nontemplate_database(self):
        calls = []
        def query(sql, database):
            calls.append(database)
            if database == 'postgres': return 'opensite\nother\nplain\n'
            return 't|t\n' if database == 'other' else 'f|f\n'
        with self.assertRaisesRegex(ops.DeployError, 'SECOND_APPLICATION_DATABASE'):
            ops.preflight('opensite', query)
        self.assertIn('other', calls)

    def test_current_database_and_plain_databases_are_allowed(self):
        def query(sql, database):
            if database == 'postgres': return 'opensite\nplain\n'
            return 't|t\n' if database == 'opensite' else 'f|f\n'
        ops.preflight('opensite', query)

    def test_uninspectable_database_fails_closed(self):
        def query(sql, database):
            if database == 'postgres': return 'opensite\nother\n'
            raise ops.DeployError('unavailable')
        with self.assertRaises(ops.DeployError): ops.preflight('opensite', query)

    def test_connections_disabled_database_is_not_silently_skipped(self):
        def query(sql, database):
            if sql.startswith('SELECT datname'):
                return 'opensite\n' if 'datallowconn' in sql else 'opensite\nops_closed\n'
            if database == 'ops_closed':
                raise ops.DeployError('DATABASE_OPERATION_FAILED')
            return 't|t\n'
        with self.assertRaisesRegex(ops.DeployError, 'DATABASE_OPERATION_FAILED'):
            ops.preflight('opensite', query)

    def test_names_and_secrets_are_validated(self):
        for name in ('', 'bad-name', 'a;DROP DATABASE x', 'x'*64):
            with self.assertRaises(ops.DeployError): ops.database_name(name)
        for secret in ('', 'replace_me', 'bad\nvalue', 'short'):
            with self.assertRaises(ops.DeployError): ops.secret(secret)
        self.assertEqual(ops.literal("a'b\\c"), "'a''b\\c'")

    def test_system_databases_are_never_migration_targets(self):
        fetch = lambda sql, database: 'opensite\n' if sql.startswith('SELECT datname') else 'f|f\n'
        for name in ('postgres', 'template0', 'template1'):
            with self.assertRaisesRegex(ops.DeployError, 'SYSTEM_DATABASE_TARGET'):
                ops.preflight(name, fetch)
            with self.assertRaisesRegex(ops.DeployError, 'SYSTEM_DATABASE_TARGET'):
                ops.provision_sql(name, self.valid_passwords())
            with patch.dict(os.environ, {'OPENSITE_DB': name, 'POSTGRES_PASSWORD': 'nonsecret_fixture_admin_123456789'}), \
                    patch.object(ops, 'query', side_effect=AssertionError('database touched')), \
                    patch('sys.stderr', new_callable=io.StringIO) as err:
                self.assertEqual(ops.main(['install']), 1)
                self.assertIn('SYSTEM_DATABASE_TARGET', err.getvalue())

    def test_forward_migrator_refuses_system_database_before_any_migration(self):
        import subprocess, tempfile
        with tempfile.TemporaryDirectory() as tmp:
            fake = Path(tmp) / 'psql'
            # Fake server: answers the identity probe; any other statement is a write attempt.
            fake.write_text('#!/bin/sh\nsql=$(cat)\ncase "$sql" in *current_database*) printf "180006\\n%s\\n" "$FAKE_DB";; '
                            '*server_version_num*) echo 180006;; *) echo WROTE >&2; exit 9;; esac\n')
            fake.chmod(0o755)
            for name in ('postgres', 'template0', 'template1'):
                env = dict(os.environ, PATH=f'{tmp}:{os.environ["PATH"]}', DATABASE_URL='host=fake', FAKE_DB=name)
                result = subprocess.run(['bash', str(ops.ROOT / 'scripts/migrate.sh')], env=env, capture_output=True, text=True, timeout=30)
                self.assertNotEqual(result.returncode, 0, name)
                self.assertIn('system database', result.stderr)
                self.assertNotIn('WROTE', result.stderr)

    def test_duplicate_component_passwords_are_rejected(self):
        passwords = self.valid_passwords()
        kinds = list(passwords)
        passwords[kinds[1]] = passwords[kinds[0]]
        with self.assertRaisesRegex(ops.DeployError, '^DUPLICATE_LOGIN_PASSWORD$'):
            ops.provision_sql('opensite', passwords)

    def test_component_password_matching_admin_is_rejected(self):
        passwords = self.valid_passwords()
        with patch.dict(os.environ, {'POSTGRES_PASSWORD': next(iter(passwords.values()))}):
            with self.assertRaisesRegex(ops.DeployError, '^DUPLICATE_LOGIN_PASSWORD$'):
                ops.provision_sql('opensite', passwords)

    def test_duplicate_password_cli_refuses_before_audit_and_writes(self):
        passwords = self.valid_passwords()
        kinds = list(passwords)
        passwords[kinds[1]] = passwords[kinds[0]]
        env = {'POSTGRES_PASSWORD': 'nonsecret_admin_fixture_123456789',
               **{f'OPENSITE_{kind}_DB_PASSWORD': value for kind, value in passwords.items()}}
        error = io.StringIO()
        with patch.dict(os.environ, env, clear=True), patch.object(ops, 'preflight'), \
                patch.object(ops, 'audit_login_roles') as audit, \
                patch.object(ops, 'install_migrations') as install, patch.object(ops, 'query') as query, \
                patch('sys.stdout', io.StringIO()), patch('sys.stderr', error):
            self.assertEqual(ops.main(['install']), 1)
            audit.assert_not_called()
            install.assert_not_called()
            query.assert_not_called()
        self.assertEqual(error.getvalue().strip(), 'DUPLICATE_LOGIN_PASSWORD')

    def test_provisioning_revokes_public_and_limits_capabilities(self):
        sql = ops.provision_sql('opensite', self.valid_passwords())
        self.assertIn('REVOKE CONNECT ON DATABASE "opensite" FROM PUBLIC', sql)
        self.assertIn('NOBYPASSRLS', sql)
        self.assertIn('NOCREATEROLE', sql)
        self.assertIn('SET log_statement', sql)
        self.assertNotIn('GRANT app_auth TO opensite_runtime_login', sql)
        self.assertNotIn('GRANT app_runtime TO opensite_auth_login', sql)

    def test_tainted_reused_login_is_refused(self):
        calls = []
        def fetch(sql, database):
            calls.append((sql, database))
            return 't\n'
        with self.assertRaisesRegex(ops.DeployError, 'TAINTED_LOGIN_ROLE'):
            ops.audit_login_roles('opensite', fetch)
        self.assertEqual(calls[0][1], 'postgres')

    def test_clean_and_uncreated_login_catalog_is_accepted(self):
        ops.audit_login_roles('opensite', lambda sql, database: 'f\n')

    def test_unknown_catalog_result_fails_closed(self):
        with self.assertRaisesRegex(ops.DeployError, 'DATABASE_INSPECTION_FAILED'):
            ops.audit_login_roles('opensite', lambda sql, database: '')

    def test_login_targeted_policy_is_rejected_by_prewrite_audit(self):
        # Fixture catalog has an RLS policy targeting an existing app login.
        # This classifier tests the selected dependency set, not PostgreSQL itself.
        def policy_catalog(sql, database):
            self.assertEqual(database, 'postgres')
            selected = re.search(r"d\.deptype IN \(([^)]*)\)", sql)
            self.assertIsNotNone(selected)
            dependency_types = re.findall(r"'([^']+)'", selected.group(1))
            return 't' if 'r' in dependency_types else 'f'
        with self.assertRaisesRegex(ops.DeployError, 'TAINTED_LOGIN_ROLE'):
            ops.audit_login_roles('opensite', policy_catalog)

    def test_provisioning_transaction_guard_includes_policy_targets(self):
        sql = ops.provision_sql('opensite', self.valid_passwords())
        guard = sql.split('DO $login_audit$', 1)[1].split('END $login_audit$;', 1)[0]
        self.assertRegex(guard, r"d\.deptype IN \([^)]*'r'")
        self.assertIn('TAINTED_LOGIN_ROLE', guard)
        self.assertLess(sql.index('DO $login_audit$'), sql.index('REVOKE CONNECT'))

    def test_audit_precedes_password_acl_and_membership_writes(self):
        sql = ops.provision_sql('opensite', self.valid_passwords())
        guard = sql.index('TAINTED_LOGIN_ROLE')
        for mutation in ('REVOKE CONNECT', 'CREATE ROLE', 'ALTER ROLE', 'DO $membership$'):
            self.assertLess(guard, sql.index(mutation))
        self.assertIn('pg_shdepend', sql)
        self.assertIn('aclexplode', sql)
        self.assertIn('is_grantable', sql)

    def test_install_refuses_tainted_login_before_migration_or_provision_writes(self):
        env = {'POSTGRES_PASSWORD': 'temporary_admin_secret_123456',
               **{f'OPENSITE_{kind}_DB_PASSWORD': value for kind, value in self.valid_passwords().items()}}
        with patch.dict(os.environ, env, clear=True), patch.object(ops, 'preflight'), \
                patch.object(ops, 'audit_login_roles', create=True, side_effect=ops.DeployError('TAINTED_LOGIN_ROLE')), \
                patch.object(ops, 'install_migrations') as install, patch.object(ops, 'query') as query, \
                patch('sys.stdout', io.StringIO()), patch('sys.stderr', io.StringIO()):
            self.assertEqual(ops.main(['install']), 1)
            install.assert_not_called()
            query.assert_not_called()

if __name__ == '__main__': unittest.main()
