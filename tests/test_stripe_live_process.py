#!/usr/bin/env python3
"""Normal-CI real-process canaries: shared Go probe + actual public wrapper.
The binary has the compile-time stripe_live_offline fake transport only. No live
call or credentials; owner entry rejection is checked separately. The output
canaries select the offline entry using a test-only launcher, never a live seam.
"""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
CANARIES = ('https://invoice.example.test/private-canary', 'owner-canary@example.test',
            'private-canary@example.test', 'pi_full_private_identifier',
            'in_full_private_identifier', 're_full_private_identifier',
            'pi_offlinepg', 'in_offlinepg', 're_offlinepg', 'acct_offlinepg')


class LiveRealProcessCanaries(unittest.TestCase):
    def test_transport_guard_in_real_and_offline_binaries(self):
        for name in ('stripe-live-compile.test', 'stripe-live.test'):
            with self.subTest(binary=name):
                binary = Path(os.environ['RUNNER_TEMP']) / name
                env = dict(os.environ, STRIPE_LIVE_OFFLINE_CASE='success')
                result = subprocess.run([str(binary), '-test.run=^TestStripeLiveTransportGuard$', '-test.v'],
                                        cwd=ROOT / 'internal/db', env=env, stdout=subprocess.PIPE,
                                        stderr=subprocess.STDOUT, text=True, timeout=30, check=False)
                self.assertEqual(result.returncode, 0, 'transport guard regression failed')
                self.assertIn('--- PASS: TestStripeLiveTransportGuard', result.stdout,
                              'transport guard test was not executed')
                self.assertNotIn('OSC_LIVE_SUMMARY ', result.stdout, 'guard test executed the probe')

    def test_owner_entry_refuses_offline_transport(self):
        binary = Path(os.environ['RUNNER_TEMP']) / 'stripe-live.test'
        env = dict(os.environ, GITHUB_ACTIONS='true', GITHUB_EVENT_NAME='workflow_dispatch',
                   GITHUB_REF='refs/heads/main', GITHUB_RUN_ATTEMPT='1',
                   STRIPE_SMOKE_PHASE='A', STRIPE_SMOKE_RUN_ID='246810',
                   STRIPE_SMOKE_ACCOUNT='acct_offlinepg',
                   STRIPE_API='rk_live_OFFLINE_NOT_A_REAL_CREDENTIAL',
                   STRIPE_LIVE_OFFLINE_CASE='success', OPENSITE_DISPOSABLE_PG='stripe-live')
        result = subprocess.run([str(binary), '-test.run=^TestStripeLiveOwnerGate$', '-test.timeout=2m'],
                                cwd=ROOT / 'internal/db', env=env, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, text=True, timeout=130, check=False)
        self.assertNotEqual(result.returncode, 0, 'owner entry admitted an offline transport')
        self.assertNotIn('OSC_LIVE_SUMMARY ', result.stdout, 'owner entry reached probe execution')

    def test_real_go_entry_all_public_sinks(self):
        binary = Path(os.environ['RUNNER_TEMP']) / 'stripe-live.test'
        self.assertTrue(binary.is_file(), 'offline binary must be compiled by CI')
        for case in ('success', 'failure', 'panic'):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as tmp:
                summary = Path(tmp) / 'summary'
                # The production wrapper's owner entry must refuse this binary.
                # This private launcher changes only the test selector to the
                # shared fake-only probe. Wrapper sinks/CWD/timeouts stay real.
                launcher = Path(tmp) / 'stripe-live.test'
                launcher.write_text('#!' + sys.executable + '\nimport os, sys\n'
                                    'assert sys.argv[1:] == ["-test.run=^TestStripeLiveOwnerGate$", "-test.timeout=2m"]\n'
                                    'os.execv(' + repr(str(binary)) + ', [' + repr(str(binary)) + ', '
                                    '"-test.run=^TestStripeLiveOfflineOutputGate$", "-test.timeout=2m"])\n')
                launcher.chmod(0o700)
                env = dict(os.environ, GITHUB_ACTIONS='true', GITHUB_EVENT_NAME='workflow_dispatch',
                           GITHUB_REF='refs/heads/main', GITHUB_RUN_ID='246810', GITHUB_RUN_ATTEMPT='1',
                           GITHUB_SHA='a' * 40, STRIPE_SMOKE_PHASE='A', STRIPE_SMOKE_RUN_ID='246810',
                           STRIPE_SMOKE_ACCOUNT='acct_offlinepg',
                           STRIPE_API='rk_live_OFFLINE_NOT_A_REAL_CREDENTIAL',
                           STRIPE_LIVE_OFFLINE_CASE=case, OPENSITE_DISPOSABLE_PG='stripe-live',
                           GITHUB_STEP_SUMMARY=str(summary), RUNNER_TEMP=tmp)
                result = subprocess.run([sys.executable, str(ROOT / 'scripts/stripe_live_ci.py')],
                                        env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                        text=True, timeout=120, check=False)
                public = result.stdout + result.stderr + (summary.read_text() if summary.exists() else '')
                for canary in CANARIES:
                    self.assertFalse(canary in public, 'public process sink leaked a private canary')
                self.assertEqual(result.returncode == 0, case == 'success', 'real child outcome was misreported')
                self.assertIn('original_run=246810', public)
                if case == 'success':
                    receipt = next(json.loads(line) for line in result.stdout.splitlines() if line.startswith('{'))
                    self.assertEqual(receipt['counts'], dict(capture_journals=0, refund_journals=0, extra_replay_journals=0))


if __name__ == '__main__':
    unittest.main()
