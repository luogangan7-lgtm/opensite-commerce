"""Customer authentication contract regression; executed only by CI check_all.

Customer identity must remain tenant-scoped and independent of staff cookies.
This is structural evidence; runtime/SQL tests provide the security proof.
"""
import json
from pathlib import Path
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[1]


class CustomerAuthContractTests(unittest.TestCase):
    def test_separate_cookie_and_complete_tenant_operations(self):
        api = yaml.safe_load((ROOT / 'api/openapi.yaml').read_text())
        scheme = api['components']['securitySchemes'].get('customerSession')
        self.assertIsNotNone(scheme, 'customer cookie must be separate from staff')
        self.assertEqual(scheme['name'], '__Host-osc_customer_session')
        base = '/api/v1/t/{tenant_id}/customer'
        expected = {
            (base + '/registrations', 'post'): 'registerCustomer',
            (base + '/verifications', 'post'): 'verifyCustomerEmail',
            (base + '/session', 'get'): 'getCustomerSession',
            (base + '/session', 'post'): 'createCustomerSession',
            (base + '/session', 'delete'): 'deleteCustomerSession',
            (base + '/password-resets', 'post'): 'requestCustomerPasswordReset',
            (base + '/password-resets/confirm', 'post'): 'confirmCustomerPasswordReset',
        }
        registry = json.loads((ROOT / 'modules/registry.yaml').read_text())
        module = next(m for m in registry['modules'] if m['name'] == 'customer')
        owned = {op['operation_id'] for op in module['operations']}
        for (path, method), name in expected.items():
            with self.subTest(operation=name):
                op = api['paths'].get(path, {}).get(method)
                self.assertIsNotNone(op)
                self.assertEqual(op['operationId'], name)
                self.assertEqual(op['x-opensite-status'], 'implemented')
                self.assertIn(name, owned)
                self.assertNotIn('x-required-scope', op)
                if method != 'get':
                    self.assertIn('403', op['responses'])
                    self.assertIn('429', op['responses'])

    def test_forward_migration_and_no_plaintext_secrets(self):
        migrations = list((ROOT / 'migrations').glob('*_customer_auth.sql'))
        self.assertEqual(len(migrations), 1)
        migration = migrations[0].read_text()
        for table in ('customer_auth_tokens', 'customer_sessions', 'customer_guest_order_links'):
            with self.subTest(table=table):
                self.assertIn('app.' + table, migration)
                self.assertIn('FORCE ROW LEVEL SECURITY', migration)
        self.assertIn('token_hash', migration)
        self.assertNotIn('token_plaintext', migration)


if __name__ == '__main__':
    unittest.main()
