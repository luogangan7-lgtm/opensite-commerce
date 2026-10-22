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
            filename = ('mvp-gaps.spec.ts' if gap else 'payments.spec.ts' if title in module.PAYMENT_TESTS
                        else 'admin.spec.ts' if title.startswith('real ') else 'storefront.spec.ts')
            specs.append({'title': title, 'file': filename, 'tests': [
                {'projectName': project, 'expectedStatus': 'skipped' if gap else 'passed',
                 'annotations': [{'type': 'fixme'}] if gap else [],
                 'results': [{'status': 'skipped' if gap else 'passed'}]}
                for project in ('desktop', 'mobile')]})
        return {'stats': {'expected': 24, 'skipped': 8, 'unexpected': 0, 'flaky': 0},
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
        self.assertEqual(module.verify_browser_report(report)['expected'], 24)
        for mutation in ('remove', 'wrong_file', 'skip'):
            changed = json.loads(json.dumps(report))
            payment = next(s for s in changed['suites'][0]['specs'] if s['title'] in module.PAYMENT_TESTS)
            if mutation == 'remove': changed['suites'][0]['specs'].remove(payment)
            elif mutation == 'wrong_file': payment['file'] = 'admin.spec.ts'
            else: payment['tests'][0]['results'] = [{'status': 'skipped'}]
            with self.subTest(mutation=mutation), self.assertRaises(RuntimeError):
                module.verify_browser_report(changed)

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


if __name__ == '__main__':
    unittest.main()
