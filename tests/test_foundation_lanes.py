#!/usr/bin/env python3
"""CI-SPEED-1: required context fails closed over every parallel matrix lane.
Depends on checks.yml and PyYAML; called by check_all.sh, never executes gates here.
"""
from pathlib import Path
import json
import subprocess
import unittest
import yaml

ROOT = Path(__file__).resolve().parents[1]
LANES = {'go', 'offline', 'pg', 'pgx', 'sqlc'}


class FoundationLanes(unittest.TestCase):
    def test_required_context_and_all_lane_results(self):
        jobs = yaml.safe_load((ROOT / '.github/workflows/checks.yml').read_text())['jobs']
        parts = jobs['foundation-lane']
        self.assertEqual(set(parts['strategy']['matrix']['lane']), LANES)
        self.assertIs(parts['strategy']['fail-fast'], False)
        foundation = jobs['foundation']
        self.assertEqual(foundation['needs'], ['foundation-lane'])
        self.assertIn('always()', foundation['if'])
        self.assertIn("github.event_name == 'pull_request'", foundation['if'])
        self.assertIn("github.ref == 'refs/heads/main'", foundation['if'])
        step, = foundation['steps']
        self.assertNotIn('if', step)
        self.assertEqual(step['env']['LANE_RESULTS'], '${{ toJSON(needs) }}')
        # Execute the actual aggregate command with every terminal state, not a copy.
        for state in ('success', 'failure', 'cancelled', 'skipped', '', 'unknown'):
            result = subprocess.run(['bash', '-e', '-o', 'pipefail', '-c', step['run']],
                                    env={'LANE_RESULTS': json.dumps({'foundation-lane': {'result': state}})},
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode == 0, state == 'success', (state, result.stderr))
        for step in parts['steps']:
            self.assertNotIn('continue-on-error', step)
            if 'run' in step:
                self.assertNotIn('cache-hit', step.get('if', ''))
        cache = next(s for s in parts['steps'] if s.get('uses', '').startswith('actions/cache@'))
        key = cache['with']['key']
        for token in ('runner.os', 'runner.arch', '1.27.2', "hashFiles('go.sum')", 'matrix.lane'):
            self.assertIn(token, key)


if __name__ == '__main__':
    unittest.main()
