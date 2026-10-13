"""CI-only harness safety: refuse external work before validating Actions context.

Runs in the foundation CI via test_ops_drill; no containers are needed for these
negative cases. A fake Docker executable witnesses any attempted side effect.
"""
import os
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


if __name__ == '__main__':
    unittest.main()
