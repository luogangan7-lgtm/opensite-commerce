"""Fresh-init HBA regressions; depends on stdlib and the Compose LOGIN contract.

The ops gate calls these tests; real authentication is exercised by ops_drill.
Only application LOGINs are restricted, using an exact quoted database name.
"""
import importlib.util
import os
from pathlib import Path
import shlex
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / 'deploy/initdb/010_opensite_connection_policy.sh'
spec = importlib.util.spec_from_file_location('ops_connection_roles', ROOT / 'scripts/ops_deploy.py')
ops = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ops)
LOGINS = ','.join(login for login, _ in ops.LOGIN_ROLES.values())
ORIGINAL = (b'# Original administrator rules must remain unchanged.\n'
            b'local all all trust\n'
            b'host all all 127.0.0.1/32 scram-sha-256\n'
            b'host all all ::1/128 scram-sha-256\n'
            b'host replication all 0.0.0.0/0 scram-sha-256\n')


class ConnectionPolicyTests(unittest.TestCase):
    def run_policy(self, directory, database='opensite', **changes):
        env = {'PATH': os.defpath, 'PGDATA': str(directory), 'POSTGRES_DB': database,
               'POSTGRES_PASSWORD': 'fixture_secret_must_never_enter_hba'}
        env.update(changes)
        return subprocess.run(['sh', str(SCRIPT)], env=env, capture_output=True,
                              text=True, timeout=10)

    def assert_policy(self, database):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            hba = directory / 'pg_hba.conf'
            hba.write_bytes(ORIGINAL)
            result = self.run_policy(directory, database)
            self.assertEqual(result.returncode, 0, result.stderr)
            actual = hba.read_bytes()
            self.assertTrue(actual.endswith(ORIGINAL), 'administrator HBA suffix changed')
            prefix = actual[:-len(ORIGINAL)].decode('ascii')
            records = [shlex.split(line, posix=False) for line in prefix.splitlines()
                       if line.strip() and not line.startswith('#')]
            target = f'"{database}"'
            expected = [
                ['local', target, LOGINS, 'scram-sha-256'],
                ['host', target, LOGINS, '0.0.0.0/0', 'scram-sha-256'],
                ['host', target, LOGINS, '::/0', 'scram-sha-256'],
                ['local', 'all', LOGINS, 'reject'],
                ['host', 'all', LOGINS, '0.0.0.0/0', 'reject'],
                ['host', 'all', LOGINS, '::/0', 'reject'],
                ['local', 'replication', LOGINS, 'reject'],
                ['host', 'replication', LOGINS, '0.0.0.0/0', 'reject'],
                ['host', 'replication', LOGINS, '::/0', 'reject'],
            ]
            self.assertEqual(records, expected)
            self.assertNotIn('fixture_secret_must_never_enter_hba', actual.decode('ascii'))
            self.assertNotIn('fixture_secret_must_never_enter_hba', result.stdout + result.stderr)
            self.assertEqual(sorted(path.name for path in directory.iterdir()), ['pg_hba.conf'])
            self.assertEqual(hba.stat().st_mode & 0o777, 0o600)

    def test_exact_application_logins_and_all_connection_lanes(self):
        self.assertEqual(len(ops.LOGIN_ROLES), 6)
        self.assert_policy('opensite')

    def test_keyword_database_names_are_exact_quoted_literals(self):
        for database in ('all', 'sameuser', 'samerole', 'replication'):
            with self.subTest(database=database):
                self.assert_policy(database)

    def test_valid_name_boundaries(self):
        for database in ('a', 'a_09', 'a' * 63):
            with self.subTest(database=database):
                self.assert_policy(database)

    def test_unsafe_database_names_fail_before_any_write(self):
        for database in ('', '9name', '_name', 'Uppercase', 'bad-name', 'a' * 64,
                         'a b', 'a\nb', 'a"b', 'a\\b', 'a,b', 'aé', 'a\x7f'):
            with self.subTest(database=database), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                hba = directory / 'pg_hba.conf'
                hba.write_bytes(ORIGINAL)
                result = self.run_policy(directory, database)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('INVALID_DATABASE_NAME', result.stderr)
                self.assertEqual(hba.read_bytes(), ORIGINAL)
                self.assertEqual(sorted(path.name for path in directory.iterdir()), ['pg_hba.conf'])

    def test_missing_data_directory_or_hba_is_refused(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            for pgdata in ('', str(directory)):
                with self.subTest(pgdata=pgdata):
                    result = self.run_policy(directory, PGDATA=pgdata)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn('MISSING_HBA_FILE', result.stderr)
                    self.assertEqual(list(directory.iterdir()), [])

    def test_hba_symlink_is_refused_without_changing_target(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            original = directory / 'original'
            original.write_bytes(ORIGINAL)
            (directory / 'pg_hba.conf').symlink_to(original)
            result = self.run_policy(directory)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('MISSING_HBA_FILE', result.stderr)
            self.assertEqual(original.read_bytes(), ORIGINAL)
            self.assertTrue((directory / 'pg_hba.conf').is_symlink())

    def test_initdb_script_is_executable_and_has_valid_posix_shell(self):
        self.assertTrue(SCRIPT.is_file())
        self.assertTrue(os.access(SCRIPT, os.X_OK), 'Docker entrypoint must execute, not source, this file')
        result = subprocess.run(['sh', '-n', str(SCRIPT)], capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_compose_mounts_policy_readonly_in_postgres_initdb(self):
        source = (ROOT / 'deploy/compose.yaml').read_text()
        postgres = source.split('  postgres:', 1)[1].split('  migrate:', 1)[0]
        mount = ('./initdb/010_opensite_connection_policy.sh:'
                 '/docker-entrypoint-initdb.d/010_opensite_connection_policy.sh:ro')
        self.assertIn('- ' + mount, postgres)
        self.assertNotIn('010_opensite_connection_policy.sh', source.split('  migrate:', 1)[1])


if __name__ == '__main__':
    unittest.main()
