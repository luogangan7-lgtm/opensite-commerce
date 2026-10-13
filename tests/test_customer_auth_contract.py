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
    def test_password_bounds_are_utf8_bytes_not_character_counts(self):
        api = yaml.safe_load((ROOT / 'api/openapi.yaml').read_text())
        for name, minimum in (
            ('CustomerRegistrationInput', 12),
            ('CustomerCredentialsInput', 1),
            ('CustomerResetConfirm', 12),
        ):
            with self.subTest(schema=name):
                field = api['components']['schemas'][name]['properties']['password']
                self.assertNotIn('minLength', field,
                                 'CUSTOMER_AUTH_UTF8_CONTRACT_RED: minLength counts characters')
                self.assertNotIn('maxLength', field,
                                 'CUSTOMER_AUTH_UTF8_CONTRACT_RED: maxLength counts characters')
                self.assertEqual(field.get('x-opensite-min-utf8-bytes'), minimum)
                self.assertEqual(field.get('x-opensite-max-utf8-bytes'), 1024)
                self.assertIn('UTF-8 bytes', field.get('description', ''))

    def test_password_contract_multibyte_boundaries_match_service_policy(self):
        api = yaml.safe_load((ROOT / 'api/openapi.yaml').read_text())
        # Values are constructed during the test; these are byte-count probes,
        # not reusable login credentials. Go runtime tests exercise admission.
        two_byte = chr(0xE9)
        four_byte = chr(0x1F642)
        examples = (
            (two_byte * 5, False, True),
            (two_byte * 6, True, True),
            (four_byte * 3, True, True),
            (two_byte * 512, True, True),
            (two_byte * 513, False, False),
            (four_byte * 256, True, True),
            (four_byte * 257, False, False),
        )
        for name, is_login in (
            ('CustomerRegistrationInput', False),
            ('CustomerCredentialsInput', True),
            ('CustomerResetConfirm', False),
        ):
            field = api['components']['schemas'][name]['properties']['password']
            self.assertIn('x-opensite-min-utf8-bytes', field,
                          'CUSTOMER_AUTH_UTF8_CONTRACT_RED: missing byte minimum')
            self.assertIn('x-opensite-max-utf8-bytes', field,
                          'CUSTOMER_AUTH_UTF8_CONTRACT_RED: missing byte maximum')
            for value, new_accepts, login_accepts in examples:
                with self.subTest(schema=name, utf8_bytes=len(value.encode('utf-8'))):
                    size = len(value.encode('utf-8'))
                    accepted = (field['x-opensite-min-utf8-bytes'] <= size
                                <= field['x-opensite-max-utf8-bytes'])
                    self.assertEqual(accepted, login_accepts if is_login else new_accepts)

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
