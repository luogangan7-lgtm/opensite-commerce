"""Drill metric witnesses must detect wrong counts, duplicates and invalid age."""
import io
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
import ops_drill


class MetricWitnessTests(unittest.TestCase):
    def check(self, pending='1', dead='1', age='2.5', extra=''):
        text = (f'opensite_outbox_pending {pending}\n'
                f'opensite_outbox_dead_letters {dead}\n'
                f'opensite_outbox_oldest_pending_age_seconds {age}\n' + extra)
        with patch.object(ops_drill, 'live_url', return_value='http://worker'), \
                patch.object(ops_drill.urllib.request, 'urlopen', return_value=io.BytesIO(text.encode())):
            ops_drill.worker_metrics()

    def test_positive_witness(self):
        self.check()

    def test_counts_are_complete_samples(self):
        for pending, dead in [('10', '1'), ('1', '11'), ('1.1', '1'), ('-1', '1')]:
            with self.subTest(pending=pending, dead=dead):
                with self.assertRaises(ops_drill.OpsError):
                    self.check(pending, dead)

    def test_duplicate_sample_is_not_a_positive_witness(self):
        with self.assertRaises(ops_drill.OpsError):
            self.check(extra='opensite_outbox_pending 10\n')

    def test_age_is_a_finite_nonnegative_sample(self):
        for age in ['NaN', 'Inf', '-1', 'invalid']:
            with self.subTest(age=age):
                with self.assertRaises(ops_drill.OpsError):
                    self.check(age=age)


class PredecessorTests(unittest.TestCase):
    def test_predecessor_is_every_migration_but_the_newest(self):
        files = [Path(f'migrations/{name}.sql') for name in ('001_init', '025_ops_metrics', '026_later_feature')]
        self.assertEqual(ops_drill.migration_predecessor(files), files[:-1])

    def test_predecessor_needs_two_migrations(self):
        with self.assertRaises(ops_drill.OpsError):
            ops_drill.migration_predecessor([Path('migrations/001_init.sql')])


class StaffLoginComposeTests(unittest.TestCase):
    """Compose must wire staff login (PR #43 thread on cmd/api/main.go:40)."""
    root = Path(__file__).resolve().parents[1]

    def test_compose_api_requires_identity_settings(self):
        import yaml
        env = yaml.safe_load((self.root / 'deploy/compose.yaml').read_text())['services']['api']['environment']
        self.assertTrue(env['OSC_ADMIN_ORIGIN'].startswith('${OPENSITE_ADMIN_ORIGIN:?'))
        self.assertTrue(env['OSC_AUTH_SIGNING_KEY_HEX'].startswith('${OPENSITE_AUTH_SIGNING_KEY:?'))
        # Staff login reuses the auth capability DSN; no second auth DSN setting.
        self.assertIn('OPENSITE_AUTH_DB_PASSWORD', env)
        self.assertNotIn('OSC_AUTH_DATABASE_URL', env)

    def test_env_examples_and_drill_supply_identity_settings(self):
        example = (self.root / '.env.example').read_text()
        drill = (self.root / 'scripts/ops_drill.py').read_text()
        for key in ('OPENSITE_ADMIN_ORIGIN', 'OPENSITE_AUTH_SIGNING_KEY'):
            self.assertIn('\n' + key + '=', example)
            self.assertIn("'" + key + "'", drill)

    def response(self, status, body):
        class Response(io.BytesIO):
            pass
        value = Response(body)
        value.status = status
        return value

    def test_login_challenge_must_be_served(self):
        with patch.object(ops_drill.urllib.request, 'urlopen', return_value=self.response(200, b'{"csrf_token":"t"}')):
            ops_drill.staff_login_reachable('http://api')
        with patch.object(ops_drill.urllib.request, 'urlopen', return_value=self.response(200, b'{}')):
            with self.assertRaises(ops_drill.OpsError):
                ops_drill.staff_login_reachable('http://api')
        missing = ops_drill.urllib.error.HTTPError('http://api/api/v1/auth/login', 404, 'Not Found', {}, None)
        with patch.object(ops_drill.urllib.request, 'urlopen', side_effect=missing):
            with self.assertRaises(OSError):
                ops_drill.staff_login_reachable('http://api')


if __name__ == '__main__':
    unittest.main()
