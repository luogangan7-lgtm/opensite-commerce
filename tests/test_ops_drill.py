"""Drill metric witnesses must detect wrong counts, duplicates and invalid age."""
import io
from email.message import Message
from pathlib import Path
import secrets
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
import ops_drill
from tests.test_mvp_e2e_runner import BrowserReceiptTests, CIOnlyRunnerTests, PurchaseWitnessTests


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


class CustomerAuthComposeTests(unittest.TestCase):
    """Customer capability and local proof delivery must be deployed together."""
    root = Path(__file__).resolve().parents[1]

    def test_deployment_drill_starts_local_proof_capture(self):
        with patch.object(ops_drill, 'compose') as compose, \
                patch.object(ops_drill, 'live_url', return_value='http://127.0.0.1:8080'), \
                patch.object(ops_drill, 'healthy'), patch.object(ops_drill, 'staff_login_reachable'):
            ops_drill.start_api()
        started = compose.call_args_list[0].args[0]
        self.assertIn('mailpit', started,
                      'CUSTOMER_LOCAL_PROOF_RED: deployment starts without capture SMTP server')

    def test_compose_provisions_eighth_coordinator_login(self):
        import yaml
        services = yaml.safe_load((self.root / 'deploy/compose.yaml').read_text())['services']
        for service in ('migrate', 'api'):
            self.assertIn('OPENSITE_RETURNS_DB_PASSWORD', services[service]['environment'],
                          'RETURNS_COMPOSE_RED: missing isolated coordinator credential')
        for service in ('postgres', 'worker'):
            self.assertNotIn('OPENSITE_RETURNS_DB_PASSWORD', services[service]['environment'])
        hba = (self.root / 'deploy/initdb/010_opensite_connection_policy.sh').read_text()
        self.assertIn('opensite_returns_login', hba, 'RETURNS_HBA_RED: coordinator can reach another database')

    def test_compose_provisions_independent_customer_auth_secret(self):
        import yaml
        services = yaml.safe_load((self.root / 'deploy/compose.yaml').read_text())['services']
        for service in ('migrate', 'api'):
            env = services[service]['environment']
            self.assertIn('OPENSITE_CUSTOMER_AUTH_DB_PASSWORD', env,
                          'CUSTOMER_AUTH_COMPOSE_RED: capability pool credential omitted')
            self.assertTrue(env['OPENSITE_CUSTOMER_AUTH_DB_PASSWORD'].startswith('${OPENSITE_CUSTOMER_AUTH_DB_PASSWORD:?'))
        for service in ('postgres', 'worker'):
            self.assertNotIn('OPENSITE_CUSTOMER_AUTH_DB_PASSWORD', services[service]['environment'])

    def test_compose_pins_customer_origin_csrf_and_local_mailbox(self):
        import yaml
        services = yaml.safe_load((self.root / 'deploy/compose.yaml').read_text())['services']
        env = services['api']['environment']
        for key in ('OPENSITE_STOREFRONT_ORIGIN', 'OPENSITE_CUSTOMER_CSRF_SECRET'):
            self.assertIn(key, env, 'CUSTOMER_AUTH_SETTINGS_RED: missing customer security setting')
            self.assertTrue(env[key].startswith('${' + key + ':?'))
        self.assertEqual(env.get('NOTIFICATIONS_SMTP_ENABLED'), 'true')
        self.assertEqual(env.get('NOTIFICATIONS_SMTP_HOST'), '127.0.0.1')
        self.assertEqual(env.get('NOTIFICATIONS_SMTP_PORT'), '1025')
        self.assertEqual(env.get('APP_ENV'), 'development')
        self.assertIn('mailpit', services, 'CUSTOMER_LOCAL_PROOF_RED: local capture mailbox omitted')
        self.assertIn('@sha256:', services['mailpit']['image'])
        self.assertEqual(services['mailpit']['environment']['MP_SMTP_RELAY_ALL'], 'false')
        self.assertEqual(services['mailpit']['network_mode'], 'service:api')
        self.assertNotIn('ports', services['mailpit'])
        for binding in services['api']['ports']:
            self.assertTrue(binding.startswith('127.0.0.1:'), 'mail capture must be loopback only')
        self.assertTrue(any(binding.endswith(':8025') for binding in services['api']['ports']))

    def test_env_example_documents_customer_security_settings(self):
        example = (self.root / '.env.example').read_text()
        for key in ('OPENSITE_CUSTOMER_AUTH_DB_PASSWORD', 'OPENSITE_STOREFRONT_ORIGIN',
                    'OPENSITE_CUSTOMER_CSRF_SECRET'):
            self.assertIn('\n' + key + '=', example)

    def test_mvp_runner_supplies_customer_origin_and_runtime_secret(self):
        import ast
        source = ast.parse((self.root / 'scripts/e2e/run.py').read_text())
        settings = {node.arg: node.value for node in ast.walk(source) if isinstance(node, ast.keyword)}
        self.assertIn('OPENSITE_STOREFRONT_ORIGIN', settings,
                      'CUSTOMER_E2E_ORIGIN_RED: required Compose setting missing from real-stack runner')
        self.assertEqual(ast.literal_eval(settings['OPENSITE_STOREFRONT_ORIGIN']), 'https://store.e2e.test')
        generated = [value for node in ast.walk(source) if isinstance(node, ast.Dict)
                     for key, value in zip(node.keys, node.values)
                     if isinstance(key, ast.Constant) and key.value == 'OPENSITE_CUSTOMER_CSRF_SECRET']
        self.assertEqual(len(generated), 1, 'CUSTOMER_E2E_SECRET_RED: no runtime-generated customer CSRF material')
        self.assertIsInstance(generated[0], ast.Call)
        self.assertEqual(ast.unparse(generated[0].func), 'secrets.token_urlsafe')
        self.assertEqual(ast.literal_eval(generated[0].args[0]), 32)


class CustomerRecoveryWitnessTests(unittest.TestCase):
    """Local proof/session witnesses must fail on a foreign or unsafe artifact."""
    tenant = 'c0500001-0000-4000-8000-000000000001'
    origin = 'https://store.drill.test'

    def message(self):
        token = secrets.token_urlsafe(32)
        recipient = 'buyer-' + secrets.token_hex(8) + '@drill.test'
        link = self.origin + '/customer/verify#tenant=' + self.tenant + '&token=' + token
        message = {'To': [{'Name': '', 'Address': recipient}],
                   'Text': 'Verify your email address using this single-use link:\n' + link}
        return message, recipient, token

    def test_only_pinned_origin_fragment_proof_is_accepted(self):
        message, recipient, token = self.message()
        with patch.dict(ops_drill.os.environ, OPENSITE_STOREFRONT_ORIGIN=self.origin):
            self.assertEqual(ops_drill.proof_from_message(message, recipient, self.tenant), token)
            for mutation in (
                {'To': [{'Address': 'foreign@drill.test'}]},
                {'To': message['To'] + [{'Address': 'other@drill.test'}]},
                {'Text': message['Text'].replace(self.origin, 'https://foreign.test')},
                {'Text': message['Text'].replace('/customer/verify#', '/customer/verify?')},
                {'Text': message['Text'] + '&tenant=' + self.tenant},
                {'Text': message['Text'].replace(token, 'malformed')},
            ):
                with self.subTest(mutation=tuple(mutation)):
                    with self.assertRaises(ops_drill.OpsError):
                        ops_drill.proof_from_message(dict(message, **mutation), recipient, self.tenant)

    def test_cookie_wire_flags_and_clearing(self):
        value = secrets.token_urlsafe(32)
        header = (ops_drill.CUSTOMER_SESSION + '=' + value
                  + '; Path=/; Secure; HttpOnly; SameSite=Lax')
        cookies = {}
        headers = Message()
        headers.add_header('Set-Cookie', header)
        ops_drill.witness_cookies(headers, cookies)
        self.assertEqual(cookies, {ops_drill.CUSTOMER_SESSION: value})
        for unsafe in (header.replace('; Secure', ''), header.replace('; HttpOnly', ''),
                       header.replace('Path=/', 'Path=/api'), header + '; Domain=localhost',
                       header.replace('SameSite=Lax', 'SameSite=None')):
            headers = Message()
            headers.add_header('Set-Cookie', unsafe)
            with self.assertRaises(ops_drill.OpsError):
                ops_drill.witness_cookies(headers, {})
        headers = Message()
        headers.add_header('Set-Cookie', header + '; Max-Age=0')
        ops_drill.witness_cookies(headers, cookies)
        self.assertEqual(cookies, {})

    def test_loopback_only_transport_refuses_before_request(self):
        for url in ('https://external.test', 'http://localhost:8025',
                    'http://127.0.0.1:8025/unexpected', 'http://user@127.0.0.1:8025'):
            with self.subTest(url=url), patch.object(ops_drill.urllib.request, 'urlopen') as request:
                with self.assertRaises(ops_drill.OpsError):
                    ops_drill.witness_json(url, '/api/v1/messages')
                request.assert_not_called()

    def test_restore_checks_live_customer_and_cart_before_db_witness(self):
        witness = {'tenant': self.tenant,
                   'customer': 'c0500002-0000-4000-8000-000000000001',
                   'session': 'c0500003-0000-4000-8000-000000000001',
                   'cart': 'c0500004-0000-4000-8000-000000000001', 'cookies': {}}
        for mismatch in ('session', 'cart'):
            current = {'customer': {'customer_id': witness['customer'], 'session_id': witness['session']}}
            view = {'id': witness['cart']}
            if mismatch == 'session':
                current['customer']['session_id'] = witness['customer']
            else:
                view['id'] = witness['customer']
            with patch.object(ops_drill, 'witness_json', side_effect=[(current, {}), (view, {})]), \
                    patch.object(ops_drill, 'psql') as database:
                with self.assertRaises(ops_drill.OpsError):
                    ops_drill.restored_customer_witness('http://127.0.0.1:8080', witness)
                database.assert_not_called()


if __name__ == '__main__':
    unittest.main()
