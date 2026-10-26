#!/usr/bin/env python3
"""CI-SPEED-1: required context fails closed over every parallel matrix lane.
Depends on checks.yml and PyYAML; called by check_all.sh, never executes gates here.
"""
from pathlib import Path
import json
import re
import subprocess
import tempfile
from unittest.mock import patch
import unittest
import yaml

ROOT = Path(__file__).resolve().parents[1]
LANES = {'go', 'offline', 'pg', 'pgx', 'sqlc'}

# A wall-clock bound is a numeric literal or a variable holding one; `sudo` may
# sit between `timeout` and the command (literal or the SUDO=(sudo) array).
_BOUND = r'(?:\d+|\$\{?[A-Za-z_]\w*\}?|"\$\{?[A-Za-z_]\w*\}?")'
_SUDO = r'(?:sudo|\$\{SUDO\[@\]\}|"\$\{SUDO\[@\]\}")'
APT_CALL_BOUND = re.compile(rf'timeout[ \t]+{_BOUND}(?:[ \t]+{_SUDO})?[ \t]+$')
SHELL_COMMENT = re.compile(r'(?:^|\s)#.*$')


def unbounded_apt_calls(body):
    """Return [(line, column, line text)] for every `apt-get` lacking a bound.

    Scans every occurrence, not one per line: `timeout 180 apt-get update &&
    apt-get install x` yields the second call as an offender.
    """
    offenders = []
    for lineno, raw in enumerate(body.splitlines(), 1):
        code = SHELL_COMMENT.sub('', raw)
        for match in re.finditer(r'apt-get', code):
            if not APT_CALL_BOUND.search(code[:match.start()]):
                offenders.append((lineno, match.start(), code.strip()))
    return offenders


class FoundationLanes(unittest.TestCase):
    def test_external_smtp_workflow_is_owner_gated(self):
        path = ROOT / '.github/workflows/smtp-external-test.yml'
        self.assertTrue(path.is_file(), 'SMTP_EXTERNAL_WORKFLOW_REQUIRED')
        document = yaml.safe_load(path.read_text())
        # PyYAML YAML 1.1 reads the unquoted key `on` as True.
        self.assertEqual(document.get('on', document.get(True)), {'workflow_dispatch': None},
                         'SMTP_EXTERNAL_DISPATCH_ONLY')
        self.assertEqual(document['permissions'], {'contents': 'read'})
        self.assertEqual(set(document['jobs']), {'smtp-test'})
        job = document['jobs']['smtp-test']
        self.assertEqual(job['environment'], 'smtp-test')
        self.assertEqual(job['if'], "github.ref == 'refs/heads/main'")
        self.assertLessEqual(job['timeout-minutes'], 5)
        consumer = job['steps'][-1]
        self.assertEqual(consumer['env']['SMTP_TEST_HOST'], '${{ secrets.SMTP_TEST_HOST }}')
        self.assertEqual(consumer['env']['SMTP_TEST_PORT'], '${{ secrets.SMTP_TEST_PORT }}')
        for other in (ROOT / '.github/workflows').glob('*'):
            if other != path and other.suffix in ('.yml', '.yaml'):
                self.assertNotIn('SMTP_TEST_', other.read_text(), 'SMTP_EXTERNAL_SECRET_ISOLATION')

    def test_production_never_composes_external_smtp(self):
        for folder in ('cmd', 'internal/bootstrap'):
            for source in (ROOT / folder).rglob('*.go'):
                if source.name.endswith('_test.go') or 'smtp-external-test' in source.parts:
                    continue
                self.assertNotIn('ExternalTestSenderFromEnv', source.read_text(),
                                 'SMTP_EXTERNAL_PRODUCTION_WIRING')


    def smtp_workflow_fixture(self, root):
        path = root / '.github/workflows/smtp-external-test.yml'
        path.parent.mkdir(parents=True)
        document = yaml.safe_load((ROOT / '.github/workflows/smtp-external-test.yml').read_text())
        consumer = document['jobs']['smtp-test']['steps'][-1]
        for key in ('SMTP_TEST_HOST', 'SMTP_TEST_PORT'):
            consumer['env'][key] = '${{ secrets.' + key + ' }}'
        return path, document

    def test_external_secret_gate_negative_fixtures(self):
        # Exercise the actual gate with each expression spelling at every scope.
        forms = ("secrets.SMTP_TEST_AUTH_CODE", "secrets['SMTP_TEST_AUTH_CODE']",
                 'secrets["SMTP_TEST_AUTH_CODE"]')
        for form in forms:
            for scope in ('workflow-env', 'job-env', 'step-env', 'step-run',
                          'job-if', 'consumer-run', 'other-workflow'):
                with self.subTest(form=form, scope=scope), tempfile.TemporaryDirectory() as directory:
                    root = Path(directory)
                    path, document = self.smtp_workflow_fixture(root)
                    job = document['jobs']['smtp-test']
                    expression = '${{ ' + form + ' }}'
                    if scope == 'workflow-env':
                        document['env'] = {'ALIAS': expression}
                    elif scope == 'job-env':
                        job['env'] = {'ALIAS': expression}
                    elif scope == 'step-env':
                        job['steps'].insert(-1, {'env': {'ALIAS': expression}, 'run': 'echo "$ALIAS"'})
                    elif scope == 'step-run':
                        job['steps'].insert(-1, {'run': 'echo ' + expression})
                    elif scope == 'job-if':
                        job['if'] += ' && ' + expression
                    elif scope == 'consumer-run':
                        job['steps'][-1]['run'] += ' ' + expression
                    else:
                        (path.parent / 'leak.yaml').write_text(yaml.safe_dump({'env': {'ALIAS': expression}}))
                    path.write_text(yaml.safe_dump(document))
                    with patch.dict(globals(), ROOT=root), self.assertRaises(AssertionError):
                        self.test_external_smtp_workflow_is_owner_gated()
        for reference in ('env.SMTP_TEST_AUTH_CODE', "env['SMTP_TEST_AUTH_CODE']",
                          'env["SMTP_TEST_AUTH_CODE"]'):
            with self.subTest(reference=reference), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                path, document = self.smtp_workflow_fixture(root)
                document['jobs']['smtp-test']['steps'].insert(-1, {'run': 'echo ${{ ' + reference + ' }}'})
                path.write_text(yaml.safe_dump(document))
                with patch.dict(globals(), ROOT=root), self.assertRaises(AssertionError):
                    self.test_external_smtp_workflow_is_owner_gated()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path, document = self.smtp_workflow_fixture(root)
            # Positive control: bracket references are legal in the sole env consumer.
            for key in document['jobs']['smtp-test']['steps'][-1]['env']:
                if key.startswith('SMTP_TEST_'):
                    secret = 'SMTP_TEST_USER' if key == 'SMTP_TEST_FROM' else key
                    document['jobs']['smtp-test']['steps'][-1]['env'][key] = "${{ secrets['" + secret + "'] }}"
            path.write_text(yaml.safe_dump(document))
            with patch.dict(globals(), ROOT=root):
                self.test_external_smtp_workflow_is_owner_gated()

    def test_external_composition_gate_negative_fixtures(self):
        paths = ('cmd/worker/leak.go', 'cmd/api/leak.go', 'cmd/new-entry/leak.go',
                 'internal/bootstrap/leak.go', 'internal/modules/notifications/leak.go',
                 'internal/modules/notifications/module.go',
                 'internal/modules/notifications/internal/service/smtp_external.go',
                 'internal/other/leak.go', 'new-package/leak.go')
        for relative in paths:
            for use in ('notifications.ExternalTestSenderFromEnv(nil)',
                        'factory := notifications.ExternalTestSenderFromEnv; _ = factory'):
                with self.subTest(path=relative, use=use), tempfile.TemporaryDirectory() as directory:
                    root = Path(directory)
                    path = root / relative
                    path.parent.mkdir(parents=True)
                    # A permitted declaration must not exempt sibling functions.
                    declaration = 'func ExternalTestSenderFromEnv() {}\n'
                    path.write_text('package example\n' + declaration + 'func leak() { ' + use + ' }\n')
                    with patch.dict(globals(), ROOT=root), self.assertRaises(AssertionError):
                        self.test_production_never_composes_external_smtp()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for relative in paths[5:7]:
                path = root / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text((ROOT / relative).read_text())
            with patch.dict(globals(), ROOT=root):
                self.test_production_never_composes_external_smtp()

    def test_existing_gate_inventory_and_fixture_order(self):
        parts = yaml.safe_load((ROOT / '.github/workflows/checks.yml').read_text())['jobs']['foundation-lane']
        steps = parts['steps']
        # Authoritative serial inventory: checks.yml@be0a6413 (including #44).
        # Existing boundaries remain whole; no command sampling/filtering.
        inventory = {
            'go test -race -count=1 -timeout=15m ./...': "matrix.lane == 'go'",
            'go vet ./...': "matrix.lane == 'go'",
            'bash scripts/check_all.sh': "matrix.lane == 'offline'",
            'python3 scripts/check_ci_pins.py': "matrix.lane == 'offline'",
            'python scripts/check_contract.py': "matrix.lane == 'offline'",
            'openapi-spec-validator api/openapi.yaml': "matrix.lane == 'offline'",
            'python3 scripts/check_oapi_codegen.py': "matrix.lane == 'offline'",
            'go tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml': "matrix.lane == 'offline'",
            './scripts/check_pg.sh': "matrix.lane == 'pg'",
            'python3 tests/test_inventory_race_sync.py': "matrix.lane == 'pg'",
            './scripts/check_pg_races.sh': "matrix.lane == 'pg'",
            './scripts/check_pg_upgrade.sh': "matrix.lane == 'pg'",
            'python3 scripts/privacy_registry.py --database --fixture': "matrix.lane == 'pg'",
            'python3 scripts/check_data_rights_canaries.py': "matrix.lane == 'pg'",
            "go test -tags pgx -count=1 -timeout=5m -v -run '^TestC02cRoleScopesMirrorPostgres$' ./internal/modules/identity": "matrix.lane == 'pgx'",
            './scripts/check_catalog_api.sh': "matrix.lane == 'pgx' || matrix.lane == 'sqlc'",
            'go vet -tags pgx ./...': "matrix.lane == 'pgx'",
            'go test -race -tags pgx -count=1 -timeout=15m ./...': "matrix.lane == 'pgx'",
            './scripts/check_codegen.sh': "matrix.lane == 'sqlc'",
        }
        for command, condition in inventory.items():
            matches = [s for s in steps if command in s.get('run', '').splitlines()]
            # The original failure-only OpenAPI handoff is deliberately retained too.
            matches = [s for s in matches if 'failure()' not in s.get('if', '')]
            self.assertEqual(len(matches), 1, command)
            self.assertEqual(matches[0].get('if'), condition, command)
        for key in ('JOBS_REQUIRE_PG', 'IDEMPOTENCY_REQUIRE_PG', 'OUTBOX_REQUIRE_PG',
                    'OPENSITE_REQUIRE_PGX_FIXTURES', 'CUSTOMER_AUTH_REQUIRE_PG'):
            self.assertEqual(parts['env'][key], '1', key)
        bodies = [s.get('run', '') for s in steps]
        prepare = next(i for i, body in enumerate(bodies) if './scripts/migrate.sh' in body)
        http = next(i for i, body in enumerate(bodies) if './scripts/check_catalog_api.sh' in body)
        auth = next(i for i, body in enumerate(bodies) if 'tests/fixtures/customer_auth.sql' in body)
        race = next(i for i, body in enumerate(bodies) if 'go test -race -tags pgx' in body)
        sqlc = next(i for i, body in enumerate(bodies) if './scripts/check_codegen.sh' in body)
        self.assertTrue(prepare < http < auth < race < sqlc)
        self.assertIn('go clean -testcache', bodies[sqlc])
        self.assertIn('git diff --exit-code -- go.mod go.sum internal/db', bodies[sqlc])
        self.assertIn('git diff --exit-code -- internal/platform/httpapi/generated/api.gen.go', '\n'.join(bodies))

    def test_integrated_pg_dependencies_and_frontend_download_retry(self):
        jobs = yaml.safe_load((ROOT / '.github/workflows/checks.yml').read_text())['jobs']
        parts = jobs['foundation-lane']
        steps = parts['steps']
        bodies = [s.get('run', '') for s in steps]
        parity = next(i for i, body in enumerate(bodies) if '^TestC02cRoleScopesMirrorPostgres$' in body)
        prepare = next(i for i, body in enumerate(bodies) if './scripts/migrate.sh' in body)
        privacy = next(i for i, body in enumerate(bodies) if 'privacy_registry.py --database --fixture' in body)
        canary = next(i for i, body in enumerate(bodies) if 'scripts/check_data_rights_canaries.py' in body)
        pg = next(i for i, body in enumerate(bodies) if './scripts/check_pg.sh' in body)
        self.assertLess(prepare, parity)
        self.assertLess(pg, privacy)
        self.assertLess(privacy, canary)
        self.assertIn('postgresql-client', bodies[prepare])
        self.assertIn('postgresql-client', bodies[pg])
        # Job env reaches every matrix lane, including all pgx/codegen consumers.
        self.assertEqual(parts['env']['C22A_DATABASE_URL'], parts['env']['C07_DATABASE_URL'])
        self.assertEqual(parts['env']['C22A_MAINTENANCE_DATABASE_URL'], parts['env']['DATABASE_URL'])
        for step in steps:
            for key in ('C22A_DATABASE_URL', 'C22A_MAINTENANCE_DATABASE_URL'):
                self.assertNotIn(key, step.get('env', {}), 'no per-step fixture override')
        prerequisite = next(s for s in jobs['frontend']['steps'] if s.get('name') == 'Bun archive extraction prerequisite')
        self.assertIn('scripts/ci_apt_install.sh', prerequisite['run'])

    def assert_apt_calls_bounded(self, body, where):
        offenders = unbounded_apt_calls(body)
        self.assertEqual(
            offenders, [],
            f'{where}: every apt-get must be immediately preceded by `timeout <n>` '
            f'(optionally `sudo`) so a stalled mirror cannot hang the lane: {offenders}')

    def test_apt_installs_are_time_bounded(self):
        # Incident 39989676720: an unbounded `apt-get update` stalled foundation-pg for 22 min.
        for workflow in sorted((ROOT / '.github/workflows').glob('*.yml')):
            document = yaml.safe_load(workflow.read_text()) or {}
            for job_name, job in (document.get('jobs') or {}).items():
                for step in job.get('steps') or []:
                    body = step.get('run')
                    if body:
                        self.assert_apt_calls_bounded(
                            body, f'{workflow.name}:{job_name}:{step.get("name", "?")}')
        helper = (ROOT / 'scripts/ci_apt_install.sh').read_text()
        # Every apt-get inside the helper too: constants alone are not a bound.
        self.assert_apt_calls_bounded(helper, 'scripts/ci_apt_install.sh')
        for token in ('UPDATE_TIMEOUT=180', 'INSTALL_TIMEOUT=300', 'ATTEMPTS=3',
                      'Acquire::http::Timeout=20', 'SKIP apt: already present'):
            self.assertIn(token, helper)

    def test_apt_bound_self_check_rejects_partial_and_missing_bound(self):
        # One bounded call must not launder an unbounded sibling on the same line.
        partial = 'timeout 180 apt-get update && apt-get install x'
        second = partial.index('apt-get install')
        self.assertEqual(
            [(line, column) for line, column, _ in unbounded_apt_calls(partial)],
            [(1, second)])
        with self.assertRaises(AssertionError):
            self.assert_apt_calls_bounded(partial, 'inline-partial')
        # Stripping the wrappers while leaving the constants must still fail.
        bare = 'apt-get update && apt-get install x'
        self.assertEqual(len(unbounded_apt_calls(bare)), 2)
        with self.assertRaises(AssertionError):
            self.assert_apt_calls_bounded(bare, 'inline-bare')

    def test_apt_bound_self_check_accepts_fully_bounded_commands(self):
        bounded = ('timeout 180 apt-get update -qq'
                   ' && timeout 300 sudo apt-get install -y x'
                   ' && timeout "$UPDATE_TIMEOUT" "${SUDO[@]}" apt-get update')
        self.assertEqual(unbounded_apt_calls(bounded), [])
        self.assert_apt_calls_bounded(bounded, 'inline-bounded')

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
            print(f'AGGREGATOR PROBE result={state!r} exit={result.returncode}', flush=True)
        empty = subprocess.run(['bash', '-e', '-o', 'pipefail', '-c', step['run']],
                               env={'LANE_RESULTS': '{}'}, capture_output=True, text=True)
        self.assertNotEqual(empty.returncode, 0, 'empty dependency set must fail closed')
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
