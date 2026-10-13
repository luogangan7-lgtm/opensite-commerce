"""Ops safety regressions: fake catalog for deterministic unit tests; real PG in ops_drill."""
import importlib.util
from pathlib import Path
import unittest
import os
import io
import re
from unittest.mock import patch
from types import SimpleNamespace

spec = importlib.util.spec_from_file_location('ops_deploy', Path(__file__).resolve().parents[1] / 'scripts/ops_deploy.py')
ops = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ops)

class DeployTests(unittest.TestCase):
    def valid_passwords(self):
        return {kind: f'nonsecret_fixture_{kind}_123456789' for kind in ops.LOGIN_ROLES}

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
