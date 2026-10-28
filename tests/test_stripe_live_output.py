#!/usr/bin/env python3
"""Normal-CI public-sink canaries for success, provider failure and runner timeout.
Dependencies: stdlib mocks and the real secret-step wrapper; no child is executed.
"""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('stripe_live_ci', ROOT / 'scripts/stripe_live_ci.py')
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)
CANARIES = ['https://invoice.example.test/private-canary', 'owner-canary@example.test',
            'pi_full_private_identifier', 'in_full_private_identifier', 'rk_live_private_canary']


class OutputCanaries(unittest.TestCase):
    def test_account_required_before_child_with_clear_owner_action(self):
        for account in ('', 'acct_', 'sk_live_private_canary', 'acct_bad\n', 'acct_' + 'x' * 124):
            with self.subTest(account=account), tempfile.TemporaryDirectory() as tmp:
                env = dict(GITHUB_ACTIONS='true', GITHUB_EVENT_NAME='workflow_dispatch',
                           GITHUB_REF='refs/heads/main', GITHUB_RUN_ID='12345', GITHUB_SHA='a' * 40,
                           GITHUB_RUN_ATTEMPT='1', STRIPE_SMOKE_RUN_ID='12345', STRIPE_SMOKE_PHASE='A',
                           STRIPE_SMOKE_ACCOUNT=account, RUNNER_TEMP=tmp,
                           GITHUB_STEP_SUMMARY=str(Path(tmp) / 'summary'))
                with patch.dict(os.environ, env, clear=True), patch.object(runner.subprocess, 'run') as child:
                    with self.assertRaisesRegex(SystemExit, 'OWNER ACTION: add Environment secret STRIPE_SMOKE_ACCOUNT=acct_') as failure:
                        runner.main()
                    child.assert_not_called()
                    self.assertFalse((Path(tmp) / 'summary').exists(), 'account refusal emitted a run receipt')
                    self.assertNotIn('sk_live_private_canary', str(failure.exception))

    def test_AB_reruns_refused_before_child(self):
        for phase in ('A', 'B'):
            for attempt in ('2', '99', '', '0', 'invalid'):
                with self.subTest(phase=phase, attempt=attempt), tempfile.TemporaryDirectory() as tmp:
                    env = dict(GITHUB_ACTIONS='true', GITHUB_EVENT_NAME='workflow_dispatch',
                               GITHUB_REF='refs/heads/main', GITHUB_RUN_ID='12345', GITHUB_SHA='a' * 40,
                               GITHUB_RUN_ATTEMPT=attempt, STRIPE_SMOKE_RUN_ID='12345', STRIPE_SMOKE_PHASE=phase,
                               RUNNER_TEMP=tmp, GITHUB_STEP_SUMMARY=str(Path(tmp) / 'summary'))
                    with patch.dict(os.environ, env, clear=True), patch.object(runner.subprocess, 'run') as child:
                        with self.assertRaises(SystemExit): runner.main()
                        child.assert_not_called()

    def test_R_requires_original_run_before_child(self):
        env = dict(GITHUB_ACTIONS='true', GITHUB_EVENT_NAME='workflow_dispatch', GITHUB_REF='refs/heads/main',
                   GITHUB_RUN_ID='12345', GITHUB_SHA='a' * 40, STRIPE_SMOKE_RUN_ID='12345', STRIPE_SMOKE_PHASE='R',
                   STRIPE_SMOKE_HAS_ORIGINAL_RUN_ID='false')
        with patch.dict(os.environ, env, clear=True), patch.object(runner.subprocess, 'run') as child:
            with self.assertRaises(SystemExit): runner.main()
            child.assert_not_called()

    def test_R_rejects_malformed_original_run(self):
        for run_id in ('', 'not-a-run', '-1', '12\n34', '1' * 33):
            env = dict(GITHUB_ACTIONS='true', GITHUB_EVENT_NAME='workflow_dispatch', GITHUB_REF='refs/heads/main',
                       GITHUB_RUN_ID='12345', GITHUB_SHA='a' * 40, STRIPE_SMOKE_RUN_ID=run_id,
                       STRIPE_SMOKE_PHASE='R', STRIPE_SMOKE_HAS_ORIGINAL_RUN_ID='true')
            with patch.dict(os.environ, env, clear=True), patch.object(runner.subprocess, 'run') as child:
                with self.assertRaises(SystemExit): runner.main()
                child.assert_not_called()

    def test_all_public_sinks(self):
        for state in ('CANCELED', 'PENDING'):
            with self.subTest(state=state), tempfile.TemporaryDirectory() as tmp:
                summary = Path(tmp) / 'summary'
                env = dict(GITHUB_ACTIONS='true', GITHUB_EVENT_NAME='workflow_dispatch',
                           GITHUB_REF='refs/heads/main', GITHUB_RUN_ID='12345', GITHUB_SHA='a' * 40,
                           GITHUB_RUN_ATTEMPT='1', STRIPE_SMOKE_RUN_ID='12345', STRIPE_SMOKE_PHASE='A', RUNNER_TEMP=tmp,
                           STRIPE_SMOKE_ACCOUNT='acct_offlinepg',
                           GITHUB_STEP_SUMMARY=str(summary))
                value = dict(phase='A', status=state, amount_minor=50, currency='USD',
                             livemode=True, owner_action_needed=False)
                raw = '\n'.join(CANARIES) + '\nOSC_LIVE_SUMMARY ' + json.dumps(value)
                result = subprocess.CompletedProcess([], int(state != 'CANCELED'), stdout=raw)
                log = io.StringIO()
                with patch.dict(os.environ, env, clear=True), patch.object(runner.subprocess, 'run', return_value=result) as child, contextlib.redirect_stdout(log):
                    try:
                        runner.main()
                    except SystemExit as error:
                        log.write(str(error))
                    self.assertEqual(child.call_args.kwargs.get('cwd'), ROOT / 'internal/db')
                public = log.getvalue() + summary.read_text()
                for canary in CANARIES:
                    self.assertFalse(canary in public, 'public output leaked a private canary')
                self.assertIn('original_run=12345', public)

    def test_timeout_never_interpolates_captured_data(self):
        with tempfile.TemporaryDirectory() as tmp:
            env = dict(GITHUB_ACTIONS='true', GITHUB_EVENT_NAME='workflow_dispatch', GITHUB_REF='refs/heads/main',
                       GITHUB_RUN_ID='12345', GITHUB_SHA='a' * 40, STRIPE_SMOKE_RUN_ID='12345',
                       STRIPE_SMOKE_PHASE='A', GITHUB_RUN_ATTEMPT='1', RUNNER_TEMP=tmp, GITHUB_STEP_SUMMARY=str(Path(tmp) / 'summary'))
            env['STRIPE_SMOKE_ACCOUNT'] = 'acct_offlinepg'
            log = io.StringIO()
            error = subprocess.TimeoutExpired(CANARIES, 1, output=' '.join(CANARIES))
            with patch.dict(os.environ, env, clear=True), patch.object(runner.subprocess, 'run', side_effect=error), contextlib.redirect_stdout(log):
                with self.assertRaises(SystemExit) as failure:
                    runner.main()
            public = log.getvalue() + str(failure.exception) + (Path(tmp) / 'summary').read_text()
            for canary in CANARIES:
                self.assertFalse(canary in public, 'public timeout output leaked a private canary')

    def test_removed_phases_and_nonzero_journals_refused(self):
        good = dict(phase='A', status='CANCELED', amount_minor=50, currency='USD', livemode=True, owner_action_needed=False)
        for phase in ('B', 'R'):
            with self.assertRaises(ValueError): runner.public_summary(dict(good, phase=phase))
        with self.assertRaises(ValueError):
            runner.public_summary(dict(good, counts=dict(capture_journals=1, refund_journals=0, extra_replay_journals=0)))

    def test_tainted_summary_values_are_rejected(self):
        good = dict(phase='A', status='CANCELED', amount_minor=50, currency='USD', livemode=True, owner_action_needed=False)
        for field in good:
            for canary in CANARIES:
                bad = dict(good); bad[field] = canary
                with self.assertRaises(ValueError): runner.public_summary(bad)
        for canary in CANARIES:
            bad = dict(good, counts={'capture_journals': canary})
            with self.assertRaises(ValueError): runner.public_summary(bad)


if __name__ == '__main__':
    unittest.main()
