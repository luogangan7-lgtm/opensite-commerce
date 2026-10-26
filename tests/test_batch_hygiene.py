#!/usr/bin/env python3
"""Offline negative controls for the b-mech CI/harness/docs batch.

Each test drives the real gate or helper with a temporary tree, a shortened
clock or a fixture file.  No PostgreSQL, network, browser, build, code
generation or Docker is used.  CI runs this file before the expensive lanes so
every control reports its own red/green in one pass.
"""
from pathlib import Path
import json
import os
import re
import signal
import subprocess
import tempfile
import time
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[1]


def run(cmd, cwd=ROOT, env=None, timeout=180):
    merged = dict(os.environ)
    if env:
        merged.update(env)
    proc = subprocess.Popen(
        cmd, cwd=cwd, env=merged, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        text=True, start_new_session=True)
    try:
        stdout, stderr = proc.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        os.killpg(proc.pid, signal.SIGKILL)
        stdout, stderr = proc.communicate()
        raise
    return subprocess.CompletedProcess(cmd, proc.returncode, stdout, stderr)


def combined(result):
    return result.stdout + result.stderr


class RoleGateOmittedConstLists(unittest.TestCase):
    """#364: an omitted const list repeats the whole list, by identifier index."""

    FIXTURES = (
        'const_omitted_list_test.go',
        'const_omitted_three_test.go',
        'const_omitted_twice_test.go',
        'const_omitted_cross_file_test.go',
        'func_const_omitted_list_test.go',
    )

    def test_gate_reports_every_omitted_list_name(self):
        result = run(
            ['go', 'run', './scripts/rolegate', '-root',
             str(ROOT / 'scripts' / 'rolegate' / 'testdata' / 'fail'), '-allow', ''],
            env={'GOPROXY': 'off', 'GOSUMDB': 'off'})
        output = combined(result)
        self.assertNotEqual(result.returncode, 0, output)
        for name in self.FIXTURES:
            self.assertRegex(output, rf'(?m)^{re.escape(name)}:\d+: cluster role SQL', name)


class SubtestTOwnership(unittest.TestCase):
    """#319: helpers called from t.Run must receive that subtest's *testing.T."""

    def test_b2b_new_quote_receives_the_calling_subtest(self):
        source = (ROOT / 'internal/bootstrap/b2b_wiring_pgx_test.go').read_text()
        self.assertRegex(source, r'newQuote := func\(t \*testing\.T\) qc\.Quote \{')
        start = source.index('newQuote := func')
        end = source.index('t.Run("inquiry uses verified host"', start)
        helper = source[start:end]
        self.assertIn('t.Helper()', helper)
        self.assertIn('f.mustActor(t, f.staff, "", true)', helper)
        lifecycle = source[source.index('t.Run("buyer lifecycle and negative authority"'):]
        self.assertRegex(lifecycle, r'q := newQuote\(t\)')

    def test_login_role_admission_apply_receives_the_subtest(self):
        source = (ROOT / 'internal/modules/identity/login_pgx_test.go').read_text()
        self.assertRegex(source, r'apply := func\(t \*testing\.T, sql string\) error \{')
        start = source.index('apply := func')
        end = source.index('for _, probe := range', start)
        helper = source[start:end]
        self.assertIn('t.Helper()', helper)
        for call in (
            'db.role.Grant(t, ctx, "app_runtime", "")',
            'db.role.Revoke(t, ctx, "app_runtime")',
            'db.role.Alter(t, ctx, sql)',
        ):
            self.assertIn(call, helper)
        probes = source[end:]
        self.assertIn('apply(t, probe.add)', probes)
        self.assertIn('apply(t, probe.remove)', probes)


class GofmtGate(unittest.TestCase):
    """#210: a real gofmt gate with an unformatted-file red control."""

    def test_unformatted_tracked_go_fails_and_formatted_passes(self):
        script = ROOT / 'scripts' / 'check_gofmt.sh'
        self.assertTrue(script.is_file(), 'scripts/check_gofmt.sh is the gofmt gate')
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            run(['git', 'init', '-q', str(root)])
            (root / 'good.go').write_text('package sample\n\nfunc Good() int {\n\treturn 1\n}\n')
            (root / 'bad.go').write_text('package sample\n\nfunc Bad( ) int {\n return 2\n}\n')
            run(['git', '-C', str(root), 'add', '.'])
            red = run(['bash', str(script), str(root)])
            self.assertNotEqual(red.returncode, 0, combined(red))
            self.assertIn('bad.go', combined(red))
            self.assertNotIn('good.go', combined(red))
            (root / 'bad.go').write_text('package sample\n\nfunc Bad() int {\n\treturn 2\n}\n')
            green = run(['bash', str(script), str(root)])
            self.assertEqual(green.returncode, 0, combined(green))

    def test_offline_lane_wires_the_gate(self):
        gate = (ROOT / 'scripts/check_all.sh').read_text()
        self.assertIn('scripts/check_gofmt.sh', gate)


class PgUrlHelper(unittest.TestCase):
    """#174: temp-database URLs must drop every decoded dbname override."""

    def url_for(self, url, database):
        helper = ROOT / 'scripts' / 'lib' / 'pg_url.sh'
        self.assertTrue(helper.is_file(), 'scripts/lib/pg_url.sh is the shared constructor')
        result = run(
            ['bash', '-c', 'source "$1"; pg_url_for_database "$2" "$3"',
             '_', str(helper), url, database])
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def test_query_only_duplicate_and_encoded_dbname_are_removed(self):
        original = 'postgres://u:p@db:5432/orig?sslmode=disable&dbname=orig&application_name=check'
        sentinel = str(original)
        self.assertEqual(
            self.url_for(original, 'tmp_upgrade'),
            'postgres://u:p@db:5432/tmp_upgrade?sslmode=disable&application_name=check')
        self.assertEqual(original, sentinel, 'the caller URL must not be mutated')
        self.assertEqual(
            self.url_for('postgres://u@db/orig?dbname=a&dbname=b&sslmode=disable', 'tmp_race'),
            'postgres://u@db/tmp_race?sslmode=disable')
        self.assertEqual(
            self.url_for('postgres://u@db/orig?db%6Eame=orig&sslmode=disable', 'tmp_encoded'),
            'postgres://u@db/tmp_encoded?sslmode=disable')
        self.assertEqual(
            self.url_for('postgres://u@db/orig?DBNAME=orig&sslmode=disable', 'tmp_case'),
            'postgres://u@db/tmp_case?sslmode=disable')

    def test_db_scripts_use_the_helper_and_assert_the_exact_database(self):
        for name in ('check_pg_upgrade.sh', 'check_pg_races.sh'):
            source = (ROOT / 'scripts' / name).read_text()
            self.assertIn('pg_url_for_database', source, name)
            self.assertIn('current_database()', source, name)


class FinalLandingGate(unittest.TestCase):
    """#5: proposal validation and final landing are distinct, exact-SHA gates."""

    def landing(self, root, *args):
        script = ROOT / 'scripts' / 'check_landing.py'
        self.assertTrue(script.is_file(), 'scripts/check_landing.py is the landing gate')
        return run(['python3', str(script), '--root', str(root), *args])

    @staticmethod
    def make_repo(temp, pending=(), numbered=(1, 2)):
        root = Path(temp) / 'repo'
        (root / 'migrations' / 'pending').mkdir(parents=True)
        for number in numbered:
            (root / 'migrations' / f'{number:03d}_landed.sql').write_text('SELECT 1;\n')
        for stable_id in pending:
            (root / 'migrations' / 'pending' / f'{stable_id}_proposal.sql').write_text(
                f'-- stable-id: {stable_id}\nSELECT 1;\n')
        return root

    def test_proposal_mode_validates_but_does_not_claim_final(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp, pending=('c22a-data-rights',))
            result = self.landing(root, '--mode', 'proposal', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40)
            self.assertEqual(result.returncode, 0, combined(result))
            self.assertIn('PROPOSAL', result.stdout)

    def test_final_mode_rejects_retained_proposals(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp, pending=('c22a-data-rights',))
            result = self.landing(root, '--mode', 'final', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('FINAL_LANDING_PENDING', combined(result))

    def test_final_mode_rejects_a_numbering_gap(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp, numbered=(1, 3))
            result = self.landing(root, '--mode', 'final', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('contiguous', combined(result))

    def test_final_mode_rejects_a_synthetic_or_stale_sha(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp)
            result = self.landing(root, '--mode', 'final', '--sha', 'b' * 40,
                                  '--expected-sha', 'a' * 40)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('FINAL_LANDING_SHA_MISMATCH', combined(result))

    def test_final_mode_writes_the_exact_sha_receipt(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp, numbered=(1, 2, 3))
            receipt = Path(temp) / 'receipt.json'
            result = self.landing(root, '--mode', 'final', '--sha', 'c' * 40,
                                  '--expected-sha', 'c' * 40, '--receipt', str(receipt))
            self.assertEqual(result.returncode, 0, combined(result))
            payload = json.loads(receipt.read_text())
            self.assertEqual(payload['mode'], 'final')
            self.assertEqual(payload['source_sha'], 'c' * 40)
            self.assertEqual(payload['pending'], 0)
            self.assertEqual(payload['numbered'], 3)

    def test_ci_binds_the_landing_check_to_the_exact_head_sha(self):
        jobs = yaml.safe_load((ROOT / '.github/workflows/checks.yml').read_text())['jobs']
        self.assertIn('final-landing', jobs)
        job = jobs['final-landing']
        self.assertEqual(job['needs'], ['foundation'])
        body = '\n'.join(step.get('run', '') for step in job['steps'])
        self.assertIn('scripts/check_landing.py', body)
        self.assertIn('github.event.pull_request.head.sha', body)
        marker = '\n'.join(step.get('env', {}).get('FINAL_LANDING', '') for step in job['steps'])
        self.assertIn('final-landing', marker)


class PendingMigrationDependency(unittest.TestCase):
    """#225: the scope union must depend on the scopes migration explicitly."""

    def test_c22a_depends_on_fulfillment_scopes(self):
        path = 'migrations/pending/c22a-data-rights_data_rights.sql'
        local = ROOT / path
        if local.is_file():
            text = local.read_text()
        else:
            # CI's ci-normalize already landed proposals in this ephemeral tree;
            # HEAD still carries the authored proposal header.
            result = run(['git', 'show', f'HEAD:{path}'])
            self.assertEqual(result.returncode, 0, result.stderr)
            text = result.stdout
        headers = text.splitlines()
        deps = [line.split(':', 1)[1].strip() for line in headers if line.startswith('-- depends-on:')]
        self.assertIn('c20a-fulfillment-scopes', deps)
        self.assertNotIn('c20a-fulfillment', deps)


class AptTotalBudget(unittest.TestCase):
    """#214: one total budget shrinks each attempt and preserves test time."""

    def test_helper_declares_an_env_overridable_total_budget(self):
        helper = (ROOT / 'scripts/ci_apt_install.sh').read_text()
        self.assertIn('CI_APT_TOTAL_BUDGET', helper)
        self.assertIn('remaining', helper)

    @staticmethod
    def fake_bins(temp):
        binder = Path(temp) / 'bin'
        binder.mkdir()
        apt = binder / 'apt-get'
        apt.write_text(
            '#!/usr/bin/env bash\n'
            'printf "%s\\n" "$*" >> "$APT_LOG"\n'
            'sleep "${APT_SLEEP:-0}"\n'
            'if [[ ${1:-} == install ]]; then\n'
            '  count=$(( $(cat "$APT_COUNT" 2>/dev/null || echo 0) + 1 ))\n'
            '  echo "$count" > "$APT_COUNT"\n'
            '  [[ $count -ge ${APT_SUCCEED_AT:-1} ]] && exit 0\n'
            '  exit 1\n'
            'fi\n'
            'exit ${APT_UPDATE_EXIT:-0}\n')
        apt.chmod(0o755)
        sudo = binder / 'sudo'
        sudo.write_text('#!/usr/bin/env bash\n[ "$1" = "-n" ] && shift\nexec "$@"\n')
        sudo.chmod(0o755)
        return binder

    def test_already_present_is_a_true_noop(self):
        with tempfile.TemporaryDirectory() as temp:
            binder = self.fake_bins(temp)
            log = Path(temp) / 'apt.log'
            result = run(
                ['bash', str(ROOT / 'scripts/ci_apt_install.sh'), 'bash'],
                env={'PATH': f'{binder}{os.pathsep}{os.environ["PATH"]}',
                     'APT_LOG': str(log)})
            self.assertEqual(result.returncode, 0, combined(result))
            self.assertIn('SKIP apt', result.stdout)
            self.assertFalse(log.exists() and log.read_text().strip())

    def test_first_failure_then_success_keeps_retrying(self):
        with tempfile.TemporaryDirectory() as temp:
            binder = self.fake_bins(temp)
            log = Path(temp) / 'apt.log'
            result = run(
                ['bash', str(ROOT / 'scripts/ci_apt_install.sh'),
                 '--no-install-recommends', 'fakepkg'],
                env={'PATH': f'{binder}{os.pathsep}{os.environ["PATH"]}',
                     'APT_LOG': str(log), 'APT_COUNT': str(Path(temp) / 'count'),
                     'APT_SUCCEED_AT': '2', 'APT_SLEEP': '0',
                     'CI_APT_TOTAL_BUDGET': '20', 'CI_APT_RETRY_BACKOFF': '0',
                     'CI_APT_UPDATE_TIMEOUT': '5', 'CI_APT_INSTALL_TIMEOUT': '5'})
            self.assertEqual(result.returncode, 0, combined(result))
            calls = log.read_text().splitlines()
            installs = [call for call in calls if call.startswith('install')]
            self.assertEqual(len(installs), 2)
            self.assertIn('--no-install-recommends', installs[0])

    def test_shrunk_total_budget_bounds_a_slow_failure(self):
        with tempfile.TemporaryDirectory() as temp:
            binder = self.fake_bins(temp)
            log = Path(temp) / 'apt.log'
            started = time.monotonic()
            try:
                result = run(
                    ['bash', str(ROOT / 'scripts/ci_apt_install.sh'), 'fakepkg'],
                    env={'PATH': f'{binder}{os.pathsep}{os.environ["PATH"]}',
                         'APT_LOG': str(log), 'APT_COUNT': str(Path(temp) / 'count'),
                         'APT_SLEEP': '10', 'APT_SUCCEED_AT': '99',
                         'CI_APT_TOTAL_BUDGET': '2', 'CI_APT_UPDATE_TIMEOUT': '1',
                         'CI_APT_INSTALL_TIMEOUT': '1', 'CI_APT_ATTEMPTS': '3',
                         'CI_APT_RETRY_BACKOFF': '0'},
                    timeout=15)
            except subprocess.TimeoutExpired:
                self.fail('ci_apt_install ignored the shrunk total budget')
            elapsed = time.monotonic() - started
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertLess(elapsed, 8, f'total budget exceeded: {elapsed:.1f}s')
            self.assertIn('total budget', combined(result))


if __name__ == '__main__':
    unittest.main(verbosity=2)
