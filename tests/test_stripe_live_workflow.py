#!/usr/bin/env python3
"""Responsibility: dispatch/environment/secret ownership gate; normal foundation CI.
Dependencies: PyYAML and committed workflows. No workflow or provider execution.
"""
from pathlib import Path
import copy
import re
import tempfile
import unittest
from unittest.mock import patch
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
    assert env.get('STRIPE_SMOKE_ACCOUNT') == '${{ secrets.STRIPE_SMOKE_ACCOUNT }}', 'LIVE_ACCOUNT_NOT_PUBLIC'
    identity = '${{ inputs.phase == \'R\' && inputs.smoke_run_id || github.run_id }}'
    assert env.get('STRIPE_SMOKE_RUN_ID') == identity, 'LIVE_PROVIDER_RUN_IDENTITY'
    assert document.get('concurrency', {}).get('group') == 'stripe-live-' + identity, 'LIVE_ORIGINAL_RUN_CONCURRENCY'
    assert env.get('STRIPE_SMOKE_HAS_ORIGINAL_RUN_ID') == "${{ inputs.smoke_run_id != '' }}", 'LIVE_R_ORIGINAL_REQUIRED'
    assert 'secrets.' not in consumers[0].get('run', ''), 'LIVE_SECRET_ARGV'
    assert consumers[0].get('run') == 'python3 scripts/stripe_live_ci.py', 'LIVE_ALLOWLISTED_OUTPUT_WRAPPER'
    assert not any('upload-artifact' in s.get('uses', '') for s in job.get('steps', [])), 'LIVE_NO_RAW_ARTIFACTS'
    assert 'stripe_live_offline' not in str(document), 'LIVE_NO_FAKE_TRANSPORT'
    for name, body in workflows.items():
        if name != LIVE.name:
            assert not re.search(r'secrets\s*(?:\.\s*(?:STRIPE_API|STRIPE_SMOKE_EMAIL)\b|\[\s*[\'"](?:STRIPE_API|STRIPE_SMOKE_EMAIL)[\'"]\s*\])', body), 'LIVE_SECRET_OWNERSHIP'
    for path in (ROOT / 'internal').rglob('*.go'):
        if path.name.endswith('_test.go') or path.name == 'stripe_live.go':
            continue
        assert 'NewStripeLiveSmoke(' not in path.read_text(), 'LIVE_NO_PRODUCTION_COMPOSITION'


class LiveWorkflowGate(unittest.TestCase):
    def test_second_secret_consumer_expression_forms(self):
        original = yaml.safe_load(LIVE.read_text())
        for secret in ('STRIPE_API', 'STRIPE_SMOKE_EMAIL', 'STRIPE_SMOKE_ACCOUNT'):
            for expression in (f'secrets.{secret}', f"secrets['{secret}']", f'secrets["{secret}"]', f"secrets [ '{secret}' ]", f'env.{secret}', f"env['{secret}']"):
                with self.subTest(expression=expression):
                    bad = copy.deepcopy(original)
                    bad['jobs']['live']['steps'].append({'env': {'ALIAS': '${{ ' + expression + ' }}'}, 'run': 'echo unsafe'})
                    with self.assertRaisesRegex(AssertionError, 'LIVE_SINGLE_SECRET_CONSUMER'):
                        validate(bad, {})

    def test_secret_ownership_all_workflow_blocks(self):
        original = yaml.safe_load(LIVE.read_text())
        for secret in ('STRIPE_API', 'STRIPE_SMOKE_EMAIL', 'STRIPE_SMOKE_ACCOUNT'):
            for expression in (f'secrets.{secret}', f"secrets['{secret}']", f'secrets["{secret}"]'):
                for block in ('workflow', 'job', 'step'):
                    with self.subTest(secret=secret, expression=expression, block=block):
                        bad = copy.deepcopy(original)
                        target = bad if block == 'workflow' else bad['jobs']['live'] if block == 'job' else bad['jobs']['live']['steps'][0]
                        target['env'] = {'ALIAS': '${{ ' + expression + ' }}'}
                        with self.assertRaises(AssertionError): validate(bad, {})
                        other = yaml.safe_dump({'jobs': {'another': {'env': {'ALIAS': '${{ ' + expression + ' }}'}}})
                        with self.assertRaisesRegex(AssertionError, 'LIVE_SECRET_OWNERSHIP'):
                            validate(original, {'other.yml': other})

    def test_production_composition_all_paths_and_symbols(self):
        original = yaml.safe_load(LIVE.read_text())
        for name in ('internal/another/stripe_live.go', 'internal/modules/payments/stripe_live.go', 'cmd/live/main.go', 'tools/live.go'):
            for call in ('payments.NewStripeLiveSmoke(c, o)', 'payments. NewStripeLiveSmoke (c, o)', 'payments.NewStripeLiveSmoke'):
                with self.subTest(path=name, call=call), tempfile.TemporaryDirectory() as tmp:
                    root = Path(tmp)
                    (root / 'internal').mkdir()
                    path = root / name
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text('package rogue\nfunc boot() { _ = ' + call + ' }\n')
                    with patch.dict(validate.__globals__, ROOT=root):
                        with self.assertRaisesRegex(AssertionError, 'LIVE_NO_PRODUCTION_COMPOSITION'):
                            validate(original, {})

    def test_constructor_declaration_only_exemption(self):
        original = yaml.safe_load(LIVE.read_text())
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            path = root / 'internal/modules/payments/stripe_live.go'
            path.parent.mkdir(parents=True)
            declaration = 'package payments\nfunc NewStripeLiveSmoke(c Config, o LiveSmokeOptions) (*StripeLiveSmoke, error) { return nil, nil }\n'
            path.write_text(declaration + '// NewStripeLiveSmoke(c, o) is forbidden here\nvar label = "NewStripeLiveSmoke(c, o)"\n')
            with patch.dict(validate.__globals__, ROOT=root):
                validate(original, {})
                path.write_text(declaration + 'func rogue() { _ = NewStripeLiveSmoke(c, o) }\n')
                with self.assertRaisesRegex(AssertionError, 'LIVE_NO_PRODUCTION_COMPOSITION'):
                    validate(original, {})

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
        bad = copy.deepcopy(original)
        bad['concurrency']['group'] = 'stripe-live-${{ inputs.smoke_run_id || github.run_id }}'
        with self.assertRaisesRegex(AssertionError, 'LIVE_ORIGINAL_RUN_CONCURRENCY'): validate(bad, {})
        for command in ('"$RUNNER_TEMP/stripe-live.test"', 'python3 scripts/stripe_live_ci.py; echo unsafe'):
            bad = copy.deepcopy(original)
            next(s for s in bad['jobs']['live']['steps'] if 'STRIPE_API' in s.get('env', {}))['run'] = command
            with self.assertRaisesRegex(AssertionError, 'LIVE_ALLOWLISTED_OUTPUT_WRAPPER'): validate(bad, {})
        bad = copy.deepcopy(original); bad['jobs']['live']['steps'].append({'uses':'actions/upload-artifact@forbidden'})
        with self.assertRaisesRegex(AssertionError, 'LIVE_NO_RAW_ARTIFACTS'): validate(bad, {})
        bad = copy.deepcopy(original); bad['jobs']['live']['steps'][2]['run'] += '\n# stripe_live_offline'
        with self.assertRaisesRegex(AssertionError, 'LIVE_NO_FAKE_TRANSPORT'): validate(bad, {})
        for secret in ('STRIPE_API','STRIPE_SMOKE_EMAIL'):
            with self.assertRaisesRegex(AssertionError,'LIVE_SECRET_OWNERSHIP'):
                validate(original, {'other.yml':'key: ${{ secrets.'+secret+' }}'})


if __name__ == '__main__':
    unittest.main()
