#!/usr/bin/env python3
"""Responsibility: Environment ownership, workflow byte lock and Go capability gate.
Dependencies: PyYAML, wrapper fixtures and Git-tracked sources; normal foundation CI.
Invariant: only the pinned live job can enter stripe-live. No provider execution.
"""
from pathlib import Path
import hashlib
import re
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import yaml
import test_stripe_live_output as output_tests

ROOT = Path(__file__).resolve().parents[1]
LIVE = ROOT / '.github/workflows/stripe-live-smoke.yml'
# Updating the reviewed workflow requires updating this pin in the same PR.
# It covers checkout ref/repository/credentials, preparation and secret consumer.
LIVE_SHA256 = 'e73a92e03f4a960410884e1c6c9b2ce8c62f8a0c970e4ff253fc87b1782090f9'
GO_NON_CODE = re.compile(r'//[^\n]*|/\*.*?\*/|"(?:\\.|[^"\\])*"|`[^`]*`|\'(?:\\.|[^\'\\])*\'', re.S)


def workflows():
    return {p.name: p.read_bytes() for p in LIVE.parent.glob('*.y*ml')}


def track_go_fixture(root):
    subprocess.run(['git', 'init', '-q', str(root)], check=True, capture_output=True, timeout=10)
    subprocess.run(['git', '-C', str(root), 'add', '.'], check=True, capture_output=True, timeout=10)


def validate(sources):
    admitted = []
    for filename, body in sources.items():
        document = yaml.safe_load(body)
        for job_id, job in document.get('jobs', {}).items():
            # A caller cannot expose the called workflow's Environment owners.
            # Refuse reusable jobs until their complete call graph is scanned.
            assert 'uses' not in job, 'LIVE_REUSABLE_WORKFLOW_UNSCANNED'
            environment = job.get('environment')
            if isinstance(environment, dict):
                environment = environment.get('name')
            assert not isinstance(environment, str) or '${{' not in environment, 'LIVE_ENVIRONMENT_LITERAL_REQUIRED'
            if isinstance(environment, str) and environment.casefold() == 'stripe-live':
                admitted.append((filename, job_id))
    assert admitted == [(LIVE.name, 'live')], 'LIVE_ENVIRONMENT_OWNERSHIP'
    assert hashlib.sha256(sources[LIVE.name]).hexdigest() == LIVE_SHA256, 'LIVE_CONTENT_LOCK'
    paths = [ROOT / name for name in subprocess.check_output(
        ['git', 'ls-files', '-z', '--', '*.go'], cwd=ROOT, text=True, timeout=10).split('\0')
        if name and not name.endswith('_test.go')]
    # Literal capability regression guard complements the runtime endpoint/form
    # allowlist. Scan every live production source, not test fixtures/canaries.
    for path in paths:
        if path.parent != ROOT / 'internal/modules/payments' or not path.name.startswith('stripe_live'):
            continue
        for match in GO_NON_CODE.finditer(path.read_text()):
            token = match[0]
            if token.startswith(('//', '/*', '`json:')):
                continue
            assert not re.search(r'(?i)(?:confirm|capture|invoice|refund|customer)', token), 'LIVE_NO_MONEY_MOVING_ENDPOINT'
            assert not re.search(r'(?i)(?:^|["`/])payment_methods?(?:/|["`])', token), 'LIVE_NO_MONEY_MOVING_ENDPOINT'
    for path in paths:
        code = GO_NON_CODE.sub(' ', path.read_text())
        if path == ROOT / 'internal/modules/payments/stripe_live.go':
            # Exempt only this function's declaration token, never its body,
            # another same-named file, a call, or a constructor function value.
            code = re.sub(r'(\bfunc\s+)NewStripeLiveSmoke(?=\s*\()', r'\1DECLARATION', code, count=1)
        assert not re.search(r'\bNewStripeLiveSmoke\b', code), 'LIVE_NO_PRODUCTION_COMPOSITION'
    return admitted


class LiveWorkflowGate(unittest.TestCase):
    def test_environment_fixture_detects_missing_ownership_assertion(self):
        # A no-op validator must fail the environment-secret ownership fixture.
        with patch.dict(validate.__globals__, validate=lambda sources: None):
            with self.assertRaises(AssertionError):
                LiveWorkflowGate().test_other_environment_cannot_read_live_environment_secrets()

    def test_reusable_jobs_cannot_hide_environment_owners(self):
        original = workflows()
        for filename in ('another.yml', 'another.yaml'):
            for uses in ('other/repo/.github/workflows/live.yml@main',
                         './.github/workflows/stripe-live-smoke.yml'):
                with self.subTest(filename=filename, uses=uses):
                    other = {'jobs': {'hidden': {'uses': uses, 'secrets': 'inherit'}}}
                    with self.assertRaisesRegex(AssertionError, 'LIVE_REUSABLE_WORKFLOW_UNSCANNED'):
                        validate({**original, filename: yaml.safe_dump(other).encode()})

    def test_removed_phase_fixture_detects_phase_guard_bypass(self):
        # Run the existing wrapper fixture against a phase-only mutation. All
        # its other inputs must be valid, or another guard hides this defect.
        runner = output_tests.runner
        source = Path(runner.__file__).read_text()
        guard = "        or phase != 'A'\n"
        self.assertEqual(source.count(guard), 1)
        namespace = {'__file__': runner.__file__, '__name__': 'phase_guard_probe'}
        exec(compile(source.replace(guard, '', 1), runner.__file__, 'exec'), namespace)
        with patch.object(runner, 'main', namespace['main']):
            with self.assertRaises(AssertionError):
                output_tests.OutputCanaries().test_removed_phase_R_refused_before_child()

    def test_every_environment_name_must_be_literal(self):
        original = workflows()
        for name in ("${{ 'stripe-live' }}", '${{ inputs.env }}', 'prefix-${{ inputs.env }}'):
            for environment in (name, {'name': name}):
                for filename in ('another.yml', 'another.yaml', LIVE.name):
                    with self.subTest(environment=environment, filename=filename):
                        document = yaml.safe_load(original[filename]) if filename in original else {'jobs': {}}
                        document['jobs']['expression'] = {'environment': environment, 'steps': [{'run': 'true'}]}
                        with self.assertRaisesRegex(AssertionError, 'LIVE_ENVIRONMENT_LITERAL_REQUIRED'):
                            validate({**original, filename: yaml.safe_dump(document).encode()})

    def test_live_runs_are_serialized_without_cancellation(self):
        document = yaml.safe_load(LIVE.read_bytes())
        self.assertEqual(document['concurrency'], {'group': 'stripe-live', 'cancel-in-progress': False})

    def test_environment_belongs_to_exactly_one_live_job(self):
        original = workflows()
        for name in ('stripe-live', 'STRIPE-LIVE', 'Stripe-Live'):
            for environment in (name, {'name': name}):
                for filename in ('another.yml', 'another.yaml'):
                    with self.subTest(environment=environment, filename=filename):
                        other = yaml.safe_dump({'jobs': {'live': {'environment': environment, 'steps': [{'run': 'true'}]}}}).encode()
                        with self.assertRaisesRegex(AssertionError, 'LIVE_ENVIRONMENT_OWNERSHIP'):
                            validate({**original, filename: other})
                bad = yaml.safe_load(original[LIVE.name])
                bad['jobs']['another'] = {'environment': environment, 'steps': [{'run': 'true'}]}
                with self.subTest(environment=environment, filename=LIVE.name):
                    with self.assertRaisesRegex(AssertionError, 'LIVE_ENVIRONMENT_OWNERSHIP'):
                        validate({**original, LIVE.name: yaml.safe_dump(bad).encode()})

    def test_missing_or_replaced_live_environment_fails(self):
        original = workflows()
        for environment in (None, 'stripe-test', {'name': 'stripe-test'}):
            with self.subTest(environment=environment):
                bad = yaml.safe_load(original[LIVE.name])
                bad['jobs']['live']['environment'] = environment
                with self.assertRaisesRegex(AssertionError, 'LIVE_ENVIRONMENT_OWNERSHIP'):
                    validate({**original, LIVE.name: yaml.safe_dump(bad).encode()})
        with self.assertRaisesRegex(AssertionError, 'LIVE_ENVIRONMENT_OWNERSHIP'):
            validate({name: body for name, body in original.items() if name != LIVE.name})

    def test_live_workflow_bytes_are_pinned(self):
        original = workflows()
        source = original[LIVE.name]
        for old, new in (
            (b'ref: ${{ github.sha }}', b'ref: other-branch'),
            (b'persist-credentials: false', b'persist-credentials: true'),
            (b'persist-credentials: false', b'persist-credentials: false\n          repository: other/repository'),
            (b'./internal/db', b'./internal/modules/payments'),
            (b'run: python3 scripts/stripe_live_ci.py', b'run: echo unsafe'),
            (b'name: stripe-live-smoke', b'name: stripe-live-smoke # byte mutation'),
        ):
            with self.subTest(mutation=new):
                self.assertIn(old, source)
                body = source.replace(old, new, 1)
                with self.assertRaisesRegex(AssertionError, 'LIVE_CONTENT_LOCK'):
                    validate({**original, LIVE.name: body})

    def test_other_environment_cannot_read_live_environment_secrets(self):
        original = workflows()
        for environment in (None, 'stripe-test', {'name': 'stripe-test'}):
            with self.subTest(environment=environment):
                other = {'jobs': {'probe': {'env': {
                    'DOT': '${{ SECRETS.STRIPE_API }}',
                    'BRACKET': "${{ Secrets['STRIPE_API'] }}",
                    'CONTEXT': '${{ toJSON(secrets) }}',
                }, 'steps': [{'run': 'true'}]}}}
                if environment is not None:
                    other['jobs']['probe']['environment'] = environment
                admitted = validate({**original, 'other.yml': yaml.safe_dump(other).encode()})
                self.assertEqual(admitted, [(LIVE.name, 'live')])
                self.assertNotIn(('other.yml', 'probe'), admitted, 'other job owns no live Environment secrets')

    def test_every_workflow_is_parsed_as_yaml(self):
        for filename in ('another.yml', 'another.yaml'):
            with self.subTest(filename=filename), self.assertRaises(yaml.YAMLError):
                validate({**workflows(), filename: b'jobs: ['})

    def test_production_composition_all_paths_and_symbols(self):
        original = workflows()
        for name in ('internal/another/stripe_live.go', 'internal/modules/payments/stripe_live.go', 'cmd/live/main.go', 'tools/live.go'):
            for call in ('payments.NewStripeLiveSmoke(c, o)', 'payments. NewStripeLiveSmoke (c, o)', 'payments.NewStripeLiveSmoke'):
                with self.subTest(path=name, call=call), tempfile.TemporaryDirectory() as tmp:
                    root = Path(tmp)
                    (root / 'internal').mkdir()
                    path = root / name
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text('package rogue\nfunc boot() { _ = ' + call + ' }\n')
                    track_go_fixture(root)
                    with patch.dict(validate.__globals__, ROOT=root):
                        with self.assertRaisesRegex(AssertionError, 'LIVE_NO_PRODUCTION_COMPOSITION'):
                            validate(original)

    def test_constructor_declaration_only_exemption(self):
        original = workflows()
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            path = root / 'internal/modules/payments/stripe_live.go'
            path.parent.mkdir(parents=True)
            declaration = 'package payments\nfunc NewStripeLiveSmoke(c Config, o LiveSmokeOptions) (*StripeLiveSmoke, error) { return nil, nil }\n'
            path.write_text(declaration + '// NewStripeLiveSmoke(c, o) is forbidden here\nvar label = "NewStripeLiveSmoke(c, o)"\n')
            track_go_fixture(root)
            with patch.dict(validate.__globals__, ROOT=root):
                validate(original)
                path.write_text(declaration + 'func rogue() { _ = NewStripeLiveSmoke(c, o) }\n')
                with self.assertRaisesRegex(AssertionError, 'LIVE_NO_PRODUCTION_COMPOSITION'):
                    validate(original)

    def test_adapter_money_moving_static_negative_fixture(self):
        original = workflows()
        path = ROOT / 'internal/modules/payments/stripe_live.go'
        source = path.read_text()
        for endpoint in ('confirm', 'capture', 'invoices', 'refunds', 'payment_methods'):
            with self.subTest(endpoint=endpoint), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                target = root / 'internal/modules/payments/stripe_live.go'
                target.parent.mkdir(parents=True)
                target.write_text(source + '\nfunc forbidden() { _ = "' + endpoint + '" }\n')
                track_go_fixture(root)
                with patch.dict(validate.__globals__, ROOT=root):
                    with self.assertRaisesRegex(AssertionError, 'LIVE_NO_MONEY_MOVING_ENDPOINT'):
                        validate(original)

    def test_composition_scan_uses_only_this_repositorys_tracked_go(self):
        original = workflows()
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            tracked = root / 'cmd/path with spaces/main.go'
            tracked.parent.mkdir(parents=True)
            tracked.write_text('package main\nfunc main() {}\n')
            track_go_fixture(root)
            nested = root / '.orchestrator/wt/another-card/main.go'
            nested.parent.mkdir(parents=True)
            nested.write_text('package main\nvar rogue = payments.NewStripeLiveSmoke\n')
            track_go_fixture(nested.parent)
            with patch.dict(validate.__globals__, ROOT=root):
                validate(original)
                tracked.write_text('package main\nvar rogue = payments.NewStripeLiveSmoke\n')
                with self.assertRaisesRegex(AssertionError, 'LIVE_NO_PRODUCTION_COMPOSITION'):
                    validate(original)

    def test_committed_workflow(self):
        self.assertTrue(LIVE.is_file(), 'LIVE_WORKFLOW_MISSING')
        validate(workflows())


if __name__ == '__main__':
    unittest.main()
