#!/usr/bin/env python3
"""Responsibility: dispatch/environment/secret ownership gate; normal foundation CI.
Dependencies: PyYAML and committed workflows. No workflow or provider execution.
"""
from pathlib import Path
import copy
import re
import unittest
import yaml

ROOT = Path(__file__).resolve().parents[1]
LIVE = ROOT / '.github/workflows/stripe-live-smoke.yml'


def validate(document, workflows):
    triggers = document.get('on', document.get(True))
    assert isinstance(triggers, dict) and set(triggers) == {'workflow_dispatch'}, 'LIVE_DISPATCH_ONLY'
    jobs = document.get('jobs', {})
    assert set(jobs) == {'live'}, 'LIVE_SINGLE_GATE'
    job = jobs['live']
    assert job.get('environment') == 'stripe-live', 'LIVE_ENVIRONMENT'
    assert job.get('if') == "github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main'", 'LIVE_MAIN_ONLY'
    assert 'secrets.' not in str(document.get('env', {})) + str(job.get('env', {})), 'LIVE_STEP_SECRETS_ONLY'
    consumers = [s for s in job.get('steps', []) if 'secrets.STRIPE_' in str(s)]
    assert len(consumers) == 1, 'LIVE_SINGLE_SECRET_CONSUMER'
    env = consumers[0].get('env', {})
    assert env.get('STRIPE_API') == '${{ secrets.STRIPE_API }}', 'LIVE_KEY_ENV_ONLY'
    assert env.get('STRIPE_SMOKE_EMAIL') == '${{ secrets.STRIPE_SMOKE_EMAIL }}', 'LIVE_EMAIL_ENV_ONLY'
    assert 'secrets.' not in consumers[0].get('run', ''), 'LIVE_SECRET_ARGV'
    for name, body in workflows.items():
        if name != LIVE.name:
            assert not re.search(r'secrets\s*(?:\.\s*(?:STRIPE_API|STRIPE_SMOKE_EMAIL)\b|\[\s*[\'"](?:STRIPE_API|STRIPE_SMOKE_EMAIL)[\'"]\s*\])', body), 'LIVE_SECRET_OWNERSHIP'


class LiveWorkflowGate(unittest.TestCase):
    def test_committed_workflow(self):
        self.assertTrue(LIVE.is_file(), 'LIVE_WORKFLOW_MISSING')
        validate(yaml.safe_load(LIVE.read_text()), {p.name:p.read_text() for p in LIVE.parent.glob('*.y*ml')})

    def test_gate_mutations(self):
        self.assertTrue(LIVE.is_file(), 'LIVE_WORKFLOW_MISSING')
        original = yaml.safe_load(LIVE.read_text())
        for trigger in ('push', 'pull_request', 'workflow_call', 'schedule'):
            bad = copy.deepcopy(original)
            bad.get('on', bad.get(True))[trigger] = {}
            with self.assertRaisesRegex(AssertionError, 'LIVE_DISPATCH_ONLY'):
                validate(bad, {})
        for field, value, message in [('environment','production','LIVE_ENVIRONMENT'),('if','true','LIVE_MAIN_ONLY')]:
            bad = copy.deepcopy(original); bad['jobs']['live'][field] = value
            with self.assertRaisesRegex(AssertionError, message): validate(bad, {})
        for secret in ('STRIPE_API','STRIPE_SMOKE_EMAIL'):
            with self.assertRaisesRegex(AssertionError,'LIVE_SECRET_OWNERSHIP'):
                validate(original, {'other.yml':'key: ${{ secrets.'+secret+' }}'})


if __name__ == '__main__':
    unittest.main()
