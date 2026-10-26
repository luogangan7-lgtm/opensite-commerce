#!/usr/bin/env python3
"""Normal-CI real-process canary gate: actual Go entry + actual public wrapper.
The binary has the compile-time stripe_live_offline fake transport only. No live
call or credentials; subprocess output is inspected, never echoed on failure.
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
    def test_real_go_entry_all_public_sinks(self):
        binary = Path(os.environ['RUNNER_TEMP']) / 'stripe-live.test'
        self.assertTrue(binary.is_file(), 'offline binary must be compiled by CI')
        for case in ('success', 'failure', 'panic'):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as tmp:
                summary = Path(tmp) / 'summary'
                env = dict(os.environ, GITHUB_ACTIONS='true', GITHUB_EVENT_NAME='workflow_dispatch',
                           GITHUB_REF='refs/heads/main', GITHUB_RUN_ID='12345', GITHUB_RUN_ATTEMPT='1',
                           GITHUB_SHA='a' * 40, STRIPE_SMOKE_PHASE='B', STRIPE_SMOKE_RUN_ID='246810',
                           STRIPE_REFUND_ATTEMPT='', STRIPE_SMOKE_ACCOUNT='acct_offlinepg',
                           STRIPE_API='rk_live_OFFLINE_NOT_A_REAL_CREDENTIAL',
                           STRIPE_SMOKE_EMAIL='owner-canary@example.test',
                           STRIPE_LIVE_OFFLINE_CASE=case, OPENSITE_DISPOSABLE_PG='stripe-live',
                           GITHUB_STEP_SUMMARY=str(summary))
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
                    self.assertEqual(receipt['counts'], dict(capture_journals=1, refund_journals=1, extra_replay_journals=0))


if __name__ == '__main__':
    unittest.main()
