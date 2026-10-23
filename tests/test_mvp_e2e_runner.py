"""CI-only harness safety: refuse external work before validating Actions context.

Runs in the foundation CI via test_ops_drill; no containers are needed for these
negative cases. A fake Docker executable witnesses any attempted side effect.
"""
import os
import importlib.util
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


class CIOnlyRunnerTests(unittest.TestCase):
    def assert_refused(self, context):
        runner = Path(__file__).resolve().parents[1] / 'scripts/e2e/run.py'
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            marker = directory / 'docker-called'
            docker = directory / 'docker'
            docker.write_text('#!/bin/sh\nprintf called > "$E2E_SIDE_EFFECT_MARKER"\nexit 99\n')
            docker.chmod(0o700)
            environment = {
                'PATH': str(directory) + os.pathsep + os.defpath,
                'E2E_SIDE_EFFECT_MARKER': str(marker),
                **context,
            }
            result = subprocess.run(
                [sys.executable, str(runner), '--receipt-dir', str(directory / 'receipt')],
                env=environment, text=True, capture_output=True, timeout=10,
            )
            self.assertEqual(result.returncode, 2, result.stderr)
            self.assertIn('GitHub Actions', result.stderr)
            self.assertFalse(marker.exists(), 'Docker ran before CI authorization')
            self.assertFalse((directory / 'receipt').exists(), 'receipt created before CI authorization')

    def test_missing_actions_context_cannot_start_stack(self):
        self.assert_refused({})

    def test_generic_ci_flag_cannot_start_stack(self):
        self.assert_refused({'CI': 'true', 'GITHUB_ACTIONS': 'false',
                             'GITHUB_RUN_ID': '123', 'GITHUB_RUN_ATTEMPT': '1'})

    def test_invalid_run_identity_cannot_start_stack(self):
        self.assert_refused({'CI': 'true', 'GITHUB_ACTIONS': 'true',
                             'GITHUB_RUN_ID': 'not-number', 'GITHUB_RUN_ATTEMPT': '1'})


class BrowserReceiptTests(unittest.TestCase):
    def verify(self, report):
        source = Path(__file__).resolve().parents[1] / 'scripts/e2e/run.py'
        spec = importlib.util.spec_from_file_location('mvp_e2e_runner', source)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module.verify_browser_report(report)

    def test_forged_pass_totals_without_executed_assertions_are_rejected(self):
        with self.assertRaises(RuntimeError):
            self.verify({'stats': {'expected': 20, 'skipped': 4, 'unexpected': 0, 'flaky': 0},
                         'suites': []})

    def test_arbitrary_skipped_assertion_cannot_become_a_declared_gap(self):
        report = {'stats': {'expected': 20, 'skipped': 4, 'unexpected': 0, 'flaky': 0},
                  'suites': [{'specs': [{'title': 'real API denies anonymous catalog reads and creation before data access',
                                         'file': 'admin.spec.ts', 'tests': [{'projectName': 'desktop',
                                         'expectedStatus': 'skipped', 'annotations': [{'type': 'fixme'}],
                                         'results': [{'status': 'skipped'}]}]}]}]}
        with self.assertRaises(RuntimeError):
            self.verify(report)

    # Imported by tests.test_ops_drill, so these witnesses run in foundation CI.
    def runner(self):
        source = Path(__file__).resolve().parents[1] / 'scripts/e2e/run.py'
        spec = importlib.util.spec_from_file_location('mvp_signed_payment_runner', source)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def browser_report(self, module):
        specs = []
        for title in module.ACTIVE_TESTS | module.GAP_TESTS:
            gap = title in module.GAP_TESTS
            filename = ('mvp-gaps.spec.ts' if gap or title in module.ADMIN_TESTS else 'purchase.spec.ts' if title in module.PURCHASE_TESTS
                        else 'payments.spec.ts' if title in module.PAYMENT_TESTS
                        else 'admin.spec.ts' if title.startswith('real ') else 'storefront.spec.ts')
            specs.append({'title': title, 'file': filename, 'tests': [
                {'projectName': project, 'expectedStatus': 'skipped' if gap else 'passed',
                 'annotations': [{'type': 'fixme'}] if gap else [],
                 'results': [{'status': 'skipped' if gap else 'passed'}]}
                for project in ('desktop', 'mobile')]})
        return {'stats': {'expected': 36, 'skipped': 0, 'unexpected': 0, 'flaky': 0},
                'suites': [{'specs': specs}]}

    def rows(self):
        rows = []
        for n, project in enumerate(('desktop', 'mobile'), 1):
            raw = json.dumps({'event_id': f'c19-e2e-{project}-captured', 'account_id': f'c19-e2e-{project}',
                              'order_id': f'c1900003-0000-4000-8000-{n:012d}', 'order_version': 1,
                              'provider_ref': f'fake_c19_e2e_{n}', 'status': 'captured',
                              'amount_minor': 270, 'currency': 'USD'}, separators=(',', ':'))
            rows.append({'project': project, 'order_state': 'paid', 'phase': 'paid', 'on_hand': 8,
                         'reserved': 0, 'inbox': 1, 'processed': 1, 'hash': hashlib.sha256(raw.encode()).hexdigest(),
                         'events': 1, 'applied': 1, 'journals': 1, 'debits': 540, 'credits': 540,
                         'cash': 270, 'receivable': 0, 'paid_outbox': 1})
        return rows

    def test_payment_is_required_in_both_projects_and_exact_spec(self):
        module = self.runner()
        report = self.browser_report(module)
        self.assertEqual(module.verify_browser_report(report)['expected'], 36)
        for mutation in ('remove', 'wrong_file', 'skip'):
            changed = json.loads(json.dumps(report))
            payment = next(s for s in changed['suites'][0]['specs'] if s['title'] in module.PAYMENT_TESTS)
            if mutation == 'remove': changed['suites'][0]['specs'].remove(payment)
            elif mutation == 'wrong_file': payment['file'] = 'admin.spec.ts'
            else: payment['tests'][0]['results'] = [{'status': 'skipped'}]
            with self.subTest(mutation=mutation), self.assertRaises(RuntimeError):
                module.verify_browser_report(changed)

    def test_real_admin_orders_cannot_be_removed_skipped_or_reclassified(self):
        module = self.runner()
        for mutation in ('remove', 'wrong_file', 'skip', 'fixme'):
            report = self.browser_report(module)
            admin = next(s for s in report['suites'][0]['specs'] if s['title'] == 'real admin orders list and detail use persisted orders and staff scopes')
            if mutation == 'remove': report['suites'][0]['specs'].remove(admin)
            elif mutation == 'wrong_file': admin['file'] = 'admin.spec.ts'
            elif mutation == 'skip': admin['tests'][0]['results'] = [{'status': 'skipped'}]
            else: admin['tests'][0]['expectedStatus'] = 'skipped'
            with self.subTest(mutation=mutation), self.assertRaises(RuntimeError):
                module.verify_browser_report(report)

    def test_staff_publication_cannot_be_removed_or_skipped(self):
        module = self.runner()
        for mutation in ('remove', 'skip'):
            report = self.browser_report(module)
            staff = next(s for s in report['suites'][0]['specs'] if s['title'] == 'real staff create SKU → media upload → publish')
            if mutation == 'remove': report['suites'][0]['specs'].remove(staff)
            else: staff['tests'][0]['results'] = [{'status': 'skipped'}]
            with self.subTest(mutation=mutation), self.assertRaises(RuntimeError):
                module.verify_browser_report(report)

    def test_browser_green_cannot_replace_persisted_payment_witness(self):
        module = self.runner()
        self.assertEqual(module.verify_payment_witness(self.rows())['status'], 'PASS')
        for key, bad in [('order_state', 'pending_payment'), ('phase', 'ready'), ('inbox', 2),
                         ('processed', 0), ('hash', '0' * 64), ('applied', 0), ('journals', 2),
                         ('cash', 540), ('credits', 539), ('reserved', 2), ('paid_outbox', 2)]:
            rows = self.rows()
            rows[0][key] = bad
            with self.subTest(key=key), self.assertRaises(RuntimeError):
                module.verify_payment_witness(rows)
        for rows in ([], self.rows()[:1], [self.rows()[0], self.rows()[0]]):
            with self.assertRaises(RuntimeError): module.verify_payment_witness(rows)



class PurchaseWitnessTests(unittest.TestCase):
    def test_browser_pass_cannot_replace_owned_order_stock_inbox_and_ledger(self):
        source = Path(__file__).resolve().parents[1] / 'scripts/e2e/purchase.py'
        spec = importlib.util.spec_from_file_location('mvp_purchase', source)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        row = {'sessions': 6, 'paid': 4, 'receipt_only': 2, 'committed_owned': 4,
               'active_owned': 2, 'on_hand': 16, 'reserved': 2, 'inbox': 6,
               'captured': 4, 'pending_refund': 2, 'processed': 4, 'pending': 2,
               'journals': 4, 'cash': 12560, 'paid_outbox': 4, 'debits': 6280, 'credits': 6280}
        self.assertEqual(module.verify(row)['status'], 'PASS')
        for key in row:
            with self.subTest(key=key), self.assertRaises(RuntimeError):
                module.verify({**row, key: row[key] + 1})
        for bad in (None, {}, [], {**row, 'debits': 0, 'credits': 0}):
            with self.assertRaises(RuntimeError):
                module.verify(bad)

    def test_registered_persistence_rejects_each_broken_invariant(self):
        source = Path(__file__).resolve().parents[1] / 'scripts/e2e/purchase.py'
        spec = importlib.util.spec_from_file_location('registered_purchase', source)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        rows = [dict(email=f'c09f2-{project}@example.test', actor=True, paid=True,
                     stock=1, journal=1, capture=1, debits=3140, credits=3140)
                for project in ('desktop', 'mobile')]
        self.assertEqual(module.verify_registered(rows)['status'], 'PASS')
        for key in ('actor', 'paid', 'stock', 'journal', 'capture', 'debits', 'email'):
            with self.subTest(key=key), self.assertRaises(RuntimeError):
                module.verify_registered([{**rows[0], key: None}, rows[1]])
        with self.assertRaises(RuntimeError):
            module.verify_registered(rows[:1])

    def test_purchase_cannot_be_removed_skipped_or_reclassified(self):
        module = BrowserReceiptTests().runner()
        for mutation in ('remove', 'wrong_file', 'skip'):
            report = BrowserReceiptTests().browser_report(module)
            purchase = next(s for s in report['suites'][0]['specs'] if s['title'] in module.PURCHASE_TESTS)
            if mutation == 'remove': report['suites'][0]['specs'].remove(purchase)
            elif mutation == 'wrong_file': purchase['file'] = 'mvp-gaps.spec.ts'
            else: purchase['tests'][0]['results'] = [{'status': 'skipped'}]
            with self.subTest(mutation=mutation), self.assertRaises(RuntimeError):
                module.verify_browser_report(report)


if __name__ == '__main__':
    unittest.main()

