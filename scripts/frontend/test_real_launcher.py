"""CI regression for launcher cleanup, using real disposable PostgreSQL DDL.

Calls the launcher entry point; replaces only migrations/seeds/business commands
with a minimal fixture. Role creation, ownership checks and cleanup use real psql.
"""
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('visual_launcher', ROOT / 'scripts/frontend/start-real-backend.py')
launcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launcher)
real_run = subprocess.run


class Provisioned(Exception):
    """Stop after successful bootstrap, before starting business processes."""


class LauncherCleanup(unittest.TestCase):
    def test_owned_bootstrap_failure_timeout_and_interrupt_allow_second_launch(self):
        self.assertEqual(os.environ.get('GITHUB_ACTIONS'), 'true')
        source = urlsplit(os.environ['DATABASE_URL'])
        self.assertIn(source.hostname, ('postgres', 'localhost', '127.0.0.1'))
        pg = dict(os.environ, PGHOST=source.hostname, PGPORT=str(source.port or 5432),
                  PGUSER=unquote(source.username or ''), PGPASSWORD=unquote(source.password or ''),
                  PGDATABASE=source.path[1:], PGCONNECT_TIMEOUT='10')

        def query(statement):
            result = real_run(['psql', '-X', '-qAt', '-v', 'ON_ERROR_STOP=1'], input=statement,
                              text=True, capture_output=True, env=pg, timeout=60)
            if result.returncode:
                raise RuntimeError('Cleanup regression SQL failed (diagnostics redacted)')
            return result.stdout.strip()

        self.assertEqual(query("SELECT count(*) FROM pg_roles WHERE rolname IN ('visual_bootstrap','visual_cleanup_runtime');"), '0')
        group_owned = query("SELECT count(*) FROM pg_roles WHERE rolname='app_admin_bootstrap';") == '0'
        if group_owned:
            query('CREATE ROLE app_admin_bootstrap;')
        (ROOT / '.work').mkdir(exist_ok=True)
        try:
            with tempfile.TemporaryDirectory(prefix='launcher-cleanup-', dir=ROOT / '.work') as directory:
                fixture = Path(directory)
                for seed in ('tests/fixtures/storefront_seed.sql', 'scripts/e2e/seed.sql'):
                    path = fixture / seed
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text('SELECT 1;')
                for failure in ('exit', 'timeout', 'interrupt'):
                    with self.subTest(failure=failure):
                        bootstraps = []

                        def command(argv, **kwargs):
                            if argv[0] == 'psql':
                                return real_run(argv, **kwargs)
                            if argv[:3] == ['go', 'run', './cmd/bootstrap-admin']:
                                self.assertEqual(query("SELECT count(*) FROM pg_roles WHERE rolname='visual_bootstrap';"), '1')
                                bootstraps.append(kwargs['env']['OPENSITE_DB'])
                                if len(bootstraps) == 1:
                                    if failure == 'exit':
                                        raise subprocess.CalledProcessError(1, ['bootstrap-admin'])
                                    if failure == 'timeout':
                                        raise subprocess.TimeoutExpired(['bootstrap-admin'], 300)
                                    raise KeyboardInterrupt
                            if argv[:2] == ['go', 'build']:
                                raise Provisioned
                            return subprocess.CompletedProcess(argv, 0)

                        with patch.object(launcher, 'ROOT', fixture), \
                             patch.object(launcher, 'LOGIN_ROLES', {'DOMAIN': ('visual_cleanup_runtime', 'unused')}), \
                             patch.object(launcher, 'provision_sql', return_value='CREATE ROLE visual_cleanup_runtime LOGIN;'), \
                             patch.object(launcher.urllib.request, 'urlopen') as ready, \
                             patch.object(launcher.subprocess, 'run', side_effect=command):
                            ready.return_value.__enter__.return_value.status = 204
                            exception = {'exit': subprocess.CalledProcessError, 'timeout': subprocess.TimeoutExpired,
                                         'interrupt': KeyboardInterrupt}[failure]
                            try:
                                with self.assertRaises(exception):
                                    launcher.main()
                                # This fails on the reviewed launcher, before any second launch.
                                self.assertEqual(query("SELECT count(*) FROM pg_roles WHERE rolname='visual_bootstrap';"), '0', failure)
                                with self.assertRaises(Provisioned):
                                    launcher.main()
                                self.assertEqual(len(bootstraps), 2)
                                self.assertNotEqual(bootstraps[0], bootstraps[1])
                                self.assertEqual(query("SELECT count(*) FROM pg_roles WHERE rolname IN ('visual_bootstrap','visual_cleanup_runtime');"), '0')
                                self.assertEqual(query("SELECT count(*) FROM pg_database WHERE datname IN (" + ','.join("'" + name + "'" for name in bootstraps) + ');'), '0')
                                print(f'VISUAL cleanup {failure}: owned role removed; second launch provisioned', flush=True)
                            finally:
                                # Harness owns these names; restore the CI cluster even on RED.
                                query('DROP ROLE IF EXISTS visual_bootstrap; DROP ROLE IF EXISTS visual_cleanup_runtime;')
        finally:
            if group_owned:
                query('DROP ROLE app_admin_bootstrap;')


if __name__ == '__main__':
    unittest.main()
