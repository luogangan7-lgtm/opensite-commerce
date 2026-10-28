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
OWNED_SECRETS = {'STRIPE_API', 'STRIPE_SMOKE_EMAIL', 'STRIPE_SMOKE_ACCOUNT'}
# Pin the whole preparation block: no extra build, shell suffix or replacement
# binary may run between this reviewed build and the sole secret consumer.
LIVE_PREPARE = '''./scripts/ci_apt_install.sh postgresql-client
python3 scripts/pending_migrations.py ci-normalize
./scripts/migrate_river.sh
./scripts/migrate.sh
go mod download
go test -c -tags pgx,stripe_live -o "$RUNNER_TEMP/stripe-live.test" ./internal/db
'''
# Closing braces inside expression string literals are not expression ends.
EXPRESSIONS = re.compile(r'''\$\{\{((?:'(?:''|[^'])*'|"(?:\\.|[^"\\])*"|[^'"}]|}(?!}))*?)\}\}''')
REFERENCES = re.compile(r'\b(secrets|env)\s*(?:\.\s*([A-Za-z_][A-Za-z_0-9]*)|\[\s*([\'"])([^\'"]+)\3\s*\])')
GO_NON_CODE = re.compile(r'//[^\n]*|/\*.*?\*/|"(?:\\.|[^"\\])*"|`[^`]*`|\'(?:\\.|[^\'\\])*\'', re.S)


def strings(value):
    if isinstance(value, dict):
        for key, item in value.items():
            yield from strings(key)
            yield from strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from strings(item)
    elif isinstance(value, str):
        yield value


def references(value, context):
    names = set()
    for text in strings(value):
        for expression in EXPRESSIONS.findall(text):
            for match in REFERENCES.finditer(expression):
                if match[1] == context:
                    name = match[2] or match[4]
                    names.add(name.upper() if context == 'secrets' else name)
            # Computed keys and whole-context serialization cannot prove secret
            # ownership. Fail closed rather than treating them as no reference.
            assert not re.search(r'\b' + context + r'\b', REFERENCES.sub('', expression)), 'LIVE_SECRET_REFERENCE'
    return names


def secret_expression(value, name):
    return isinstance(value, str) and re.fullmatch(r'\$\{\{\s*SECRETS\s*(?:\.\s*' + re.escape(name.upper()) + r'|\[\s*([\'"])' + re.escape(name.upper()) + r'\1\s*\])\s*\}\}', value.upper())


def validate(document, workflows):
    triggers = document.get('on', document.get(True))
    assert isinstance(triggers, dict) and set(triggers) == {'workflow_dispatch'}, 'LIVE_DISPATCH_ONLY'
    jobs = document.get('jobs', {})
    assert set(jobs) == {'live'}, 'LIVE_SINGLE_GATE'
    job = jobs['live']
    assert job.get('environment') == 'stripe-live', 'LIVE_ENVIRONMENT'
    assert job.get('if') == "github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main'", 'LIVE_MAIN_ONLY'
    for block in ({k:v for k,v in document.items() if k != 'jobs'}, {k:v for k,v in job.items() if k != 'steps'}):
        assert not references(block, 'secrets'), 'LIVE_STEP_SECRETS_ONLY'
        assert not references(block, 'env') & OWNED_SECRETS, 'LIVE_STEP_SECRETS_ONLY'
    consumers = [s for s in job.get('steps', []) if references(s, 'secrets') or references(s, 'env') & OWNED_SECRETS]
    assert len(consumers) == 1, 'LIVE_SINGLE_SECRET_CONSUMER'
    env = consumers[0].get('env', {})
    assert secret_expression(env.get('STRIPE_API'), 'STRIPE_API'), 'LIVE_KEY_ENV_ONLY'
    assert 'STRIPE_SMOKE_EMAIL' not in env, 'LIVE_NO_EMAIL'
    assert secret_expression(env.get('STRIPE_SMOKE_ACCOUNT'), 'STRIPE_SMOKE_ACCOUNT'), 'LIVE_ACCOUNT_NOT_PUBLIC'
    assert not references({k:v for k,v in env.items() if k not in OWNED_SECRETS}, 'secrets'), 'LIVE_SECRET_ENV_ALIAS'
    assert not references(env, 'env') & OWNED_SECRETS, 'LIVE_SECRET_ENV_ALIAS'
    assert not triggers['workflow_dispatch'] or not triggers['workflow_dispatch'].get('inputs'), 'LIVE_A_ONLY_INPUTS'
    assert env.get('STRIPE_SMOKE_PHASE') == 'A', 'LIVE_A_ONLY_PHASE'
    assert env.get('STRIPE_SMOKE_RUN_ID') == '${{ github.run_id }}', 'LIVE_PROVIDER_RUN_IDENTITY'
    assert document.get('concurrency', {}).get('group') == 'stripe-live-${{ github.run_id }}', 'LIVE_ORIGINAL_RUN_CONCURRENCY'
    assert not {'STRIPE_REFUND_ATTEMPT', 'STRIPE_SMOKE_HAS_ORIGINAL_RUN_ID'} & env.keys(), 'LIVE_NO_RECOVERY'
    assert not references({k:v for k,v in consumers[0].items() if k != 'env'}, 'secrets'), 'LIVE_SECRET_ARGV'
    assert 'uses' not in consumers[0], 'LIVE_ALLOWLISTED_OUTPUT_WRAPPER'
    assert consumers[0].get('run') == 'python3 scripts/stripe_live_ci.py', 'LIVE_ALLOWLISTED_OUTPUT_WRAPPER'
    assert not any('upload-artifact' in s.get('uses', '') for s in job.get('steps', [])), 'LIVE_NO_RAW_ARTIFACTS'
    assert 'stripe_live_offline' not in str(document), 'LIVE_NO_FAKE_TRANSPORT'
    steps = job.get('steps', [])
    assert len(steps) == 4 and steps[2].get('run') == LIVE_PREPARE and steps[3] is consumers[0], 'LIVE_PREPARE_BUILD'
    for name, body in workflows.items():
        if name != LIVE.name:
            other = yaml.safe_load(body)
            assert not references(other, 'secrets') & OWNED_SECRETS, 'LIVE_SECRET_OWNERSHIP'
            assert not references(other, 'env') & OWNED_SECRETS, 'LIVE_SECRET_OWNERSHIP'
    # Literal capability regression guard complements the runtime endpoint/form
    # allowlist. Scan every live production source, not test fixtures/canaries.
    for path in (ROOT / 'internal/modules/payments').glob('stripe_live*.go'):
        if path.name.endswith('_test.go'):
            continue
        for match in GO_NON_CODE.finditer(path.read_text()):
            token = match[0]
            if token.startswith(('//', '/*', '`json:')):
                continue
            assert not re.search(r'(?i)(?:confirm|capture|invoice|refund|customer)', token), 'LIVE_NO_MONEY_MOVING_ENDPOINT'
            assert not re.search(r'(?i)(?:^|["`/])payment_methods?(?:/|["`])', token), 'LIVE_NO_MONEY_MOVING_ENDPOINT'
    for path in ROOT.rglob('*.go'):
        if path.name.endswith('_test.go'):
            continue
        code = GO_NON_CODE.sub(' ', path.read_text())
        if path == ROOT / 'internal/modules/payments/stripe_live.go':
            # Exempt only this function's declaration token, never its body,
            # another same-named file, a call, or a constructor function value.
            code = re.sub(r'(\bfunc\s+)NewStripeLiveSmoke(?=\s*\()', r'\1DECLARATION', code, count=1)
        assert not re.search(r'\bNewStripeLiveSmoke\b', code), 'LIVE_NO_PRODUCTION_COMPOSITION'


class LiveWorkflowGate(unittest.TestCase):
    def test_environment_belongs_to_exactly_one_live_job(self):
        original = yaml.safe_load(LIVE.read_text())
        for name in ('stripe-live', 'STRIPE-LIVE', 'Stripe-Live'):
            for environment in (name, {'name': name}):
                for filename in ('another.yml', 'another.yaml'):
                    with self.subTest(environment=environment, filename=filename):
                        other = yaml.safe_dump({'jobs': {'live': {'environment': environment, 'steps': [{'run': 'true'}]}}})
                        with self.assertRaisesRegex(AssertionError, 'LIVE_ENVIRONMENT_OWNERSHIP'):
                            validate(original, {LIVE.name: LIVE.read_text(), filename: other})
                bad = copy.deepcopy(original)
                bad['jobs']['another'] = {'environment': environment, 'steps': [{'run': 'true'}]}
                with self.subTest(environment=environment, filename=LIVE.name):
                    with self.assertRaisesRegex(AssertionError, 'LIVE_ENVIRONMENT_OWNERSHIP'):
                        validate(bad, {LIVE.name: yaml.safe_dump(bad)})

    def test_live_workflow_bytes_are_pinned(self):
        source = LIVE.read_text()
        for old, new in (
            ('ref: ${{ github.sha }}', 'ref: other-branch'),
            ('persist-credentials: false', 'persist-credentials: true'),
            ('persist-credentials: false', 'persist-credentials: false\n          repository: other/repository'),
            ('./internal/db', './internal/modules/payments'),
            ('run: python3 scripts/stripe_live_ci.py', 'run: echo unsafe'),
            ('name: stripe-live-smoke', 'name: stripe-live-smoke # byte mutation'),
        ):
            with self.subTest(mutation=new):
                self.assertIn(old, source)
                body = source.replace(old, new, 1)
                with self.assertRaisesRegex(AssertionError, 'LIVE_CONTENT_LOCK'):
                    validate(yaml.safe_load(body), {LIVE.name: body})

    def test_other_environment_cannot_read_live_environment_secrets(self):
        original = yaml.safe_load(LIVE.read_text())
        for environment in (None, 'stripe-test', {'name': 'stripe-test'}):
            with self.subTest(environment=environment):
                other = {'jobs': {'probe': {'env': {
                    'DOT': '${{ SECRETS.STRIPE_API }}',
                    'BRACKET': "${{ Secrets['STRIPE_API'] }}",
                    'CONTEXT': '${{ toJSON(secrets) }}',
                }, 'steps': [{'run': 'true'}]}}}
                if environment is not None:
                    other['jobs']['probe']['environment'] = environment
                validate(original, {LIVE.name: LIVE.read_text(), 'other.yml': yaml.safe_dump(other)})

    def test_secret_ownership_is_case_insensitive(self):
        original = yaml.safe_load(LIVE.read_text())
        for secret in OWNED_SECRETS:
            for name in (secret.lower(), secret.title()):
                for expression in (f'secrets.{name}', f"secrets['{name}']", f'secrets["{name}"]'):
                    with self.subTest(expression=expression):
                        other = yaml.safe_dump({'env': {'ALIAS': '${{ ' + expression + ' }}'}})
                        with self.assertRaisesRegex(AssertionError, 'LIVE_SECRET_OWNERSHIP'):
                            validate(original, {'other.yml': other})

    def test_prepare_build_command_is_pinned(self):
        original = yaml.safe_load(LIVE.read_text())
        command = 'go test -c -tags pgx,stripe_live -o "$RUNNER_TEMP/stripe-live.test" ./internal/db'
        for replacement in (command.replace('pgx,stripe_live', 'pgx'),
                            command.replace('pgx,stripe_live', 'pgx,stripe_live,other'),
                            command.replace('./internal/db', './internal/modules/payments'),
                            command + '; echo unsafe',
                            '# ' + command):
            with self.subTest(command=replacement):
                bad = copy.deepcopy(original)
                prepare = bad['jobs']['live']['steps'][2]
                self.assertIn(command, prepare['run'])
                prepare['run'] = prepare['run'].replace(command, replacement)
                with self.assertRaisesRegex(AssertionError, 'LIVE_PREPARE_BUILD'):
                    validate(bad, {})

    def test_secret_context_and_env_aliases_fail_closed(self):
        original = yaml.safe_load(LIVE.read_text())
        for expression in ("secrets[env.SECRET_NAME]", "toJSON(secrets)", 'env.STRIPE_API', "env['STRIPE_SMOKE_EMAIL']"):
            with self.subTest(expression=expression):
                other = yaml.safe_dump({'env': {'ALIAS': '${{ ' + expression + ' }}'}})
                with self.assertRaises(AssertionError): validate(original, {'other.yml': other})
                bad = copy.deepcopy(original)
                bad['jobs']['live']['steps'].append({'run': '${{ ' + expression + ' }}'})
                with self.assertRaises(AssertionError): validate(bad, {})

    def test_second_secret_consumer_expression_forms(self):
        original = yaml.safe_load(LIVE.read_text())
        for secret in ('STRIPE_API', 'STRIPE_SMOKE_EMAIL', 'STRIPE_SMOKE_ACCOUNT'):
            for expression in (f'secrets.{secret}', f"secrets['{secret}']", f'secrets["{secret}"]', f"secrets [ '{secret}' ]", f'env.{secret}', f"env['{secret}']", "format('}} {0}', secrets['" + secret + "'])"):
                with self.subTest(expression=expression):
                    bad = copy.deepcopy(original)
                    bad['jobs']['live']['steps'].append({'env': {'ALIAS': '${{ ' + expression + ' }}'}, 'run': 'echo unsafe'})
                    with self.assertRaisesRegex(AssertionError, 'LIVE_SINGLE_SECRET_CONSUMER'):
                        validate(bad, {})

    def test_single_consumer_supports_literal_secret_expression_forms(self):
        original = yaml.safe_load(LIVE.read_text())
        for template in ("secrets['%s']", 'secrets["%s"]', "secrets [ '%s' ]"):
            for case in (str.upper, str.lower, str.title):
                bad = copy.deepcopy(original)
                env = next(step['env'] for step in bad['jobs']['live']['steps'] if 'STRIPE_API' in step.get('env', {}))
                for name in ('STRIPE_API', 'STRIPE_SMOKE_ACCOUNT'):
                    env[name] = '${{ ' + template % case(name) + ' }}'
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
                        other = yaml.safe_dump({'jobs': {'another': {'env': {'ALIAS': '${{ ' + expression + ' }}'}}}})
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

    def test_phase_A_only_contract(self):
        doc = yaml.safe_load(LIVE.read_text())
        dispatch = doc.get('on', doc.get(True))['workflow_dispatch']
        self.assertFalse(dispatch and dispatch.get('inputs'), 'LIVE_A_ONLY_INPUTS')
        env = next(s['env'] for s in doc['jobs']['live']['steps'] if 'STRIPE_API' in s.get('env', {}))
        self.assertEqual(env['STRIPE_SMOKE_PHASE'], 'A')
        self.assertNotIn('STRIPE_SMOKE_EMAIL', env)
        self.assertNotIn('STRIPE_REFUND_ATTEMPT', env)

    def test_adapter_money_moving_static_negative_fixture(self):
        original = yaml.safe_load(LIVE.read_text())
        path = ROOT / 'internal/modules/payments/stripe_live.go'
        source = path.read_text()
        for endpoint in ('confirm', 'capture', 'invoices', 'refunds', 'payment_methods'):
            with self.subTest(endpoint=endpoint), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                target = root / 'internal/modules/payments/stripe_live.go'
                target.parent.mkdir(parents=True)
                target.write_text(source + '\nfunc forbidden() { _ = "' + endpoint + '" }\n')
                with patch.dict(validate.__globals__, ROOT=root):
                    with self.assertRaisesRegex(AssertionError, 'LIVE_NO_MONEY_MOVING_ENDPOINT'):
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
