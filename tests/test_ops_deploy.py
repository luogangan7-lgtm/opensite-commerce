"""Ops safety regressions: fake catalog for deterministic unit tests; real PG in ops_drill."""
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('ops_deploy', Path(__file__).resolve().parents[1] / 'scripts/ops_deploy.py')
ops = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ops)

class DeployTests(unittest.TestCase):
    def test_preflight_checks_every_connectable_database(self):
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

    def test_names_and_secrets_are_validated(self):
        for name in ('', 'bad-name', 'a;DROP DATABASE x', 'x'*64):
            with self.assertRaises(ops.DeployError): ops.database_name(name)
        for secret in ('', 'replace_me', 'bad\nvalue', 'short'):
            with self.assertRaises(ops.DeployError): ops.secret(secret)
        self.assertEqual(ops.literal("a'b\\c"), "'a''b\\c'")

    def test_provisioning_revokes_public_and_limits_capabilities(self):
        sql = ops.provision_sql('opensite', {kind: 'temporary_test_password_1234' for kind in ops.LOGIN_ROLES})
        self.assertIn('REVOKE CONNECT ON DATABASE "opensite" FROM PUBLIC', sql)
        self.assertIn('NOBYPASSRLS', sql)
        self.assertIn('NOCREATEROLE', sql)
        self.assertIn('SET log_statement', sql)
        self.assertNotIn('GRANT app_auth TO opensite_runtime_login', sql)
        self.assertNotIn('GRANT app_runtime TO opensite_auth_login', sql)

if __name__ == '__main__': unittest.main()
