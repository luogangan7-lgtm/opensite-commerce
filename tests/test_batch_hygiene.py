#!/usr/bin/env python3
"""Offline negative controls for the b-mech CI/harness/docs batch.

Each test drives the real gate or helper with a temporary tree, a shortened
clock or a fixture file.  No PostgreSQL, network, browser, build, code
generation or Docker is used.  CI runs this file before the expensive lanes so
every control reports its own red/green in one pass.
"""
from fnmatch import fnmatch
from pathlib import Path
import json
import os
import re
import signal
import subprocess
import sys
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

    def test_query_only_uris_preserve_the_authority(self):
        # libpq allows the database to be supplied only as `?dbname=`; the path
        # may be absent with or without the trailing slash. The host/authority
        # must survive so the URI still points at the same server.
        self.assertEqual(
            self.url_for('postgres://u@db/?dbname=orig', 'tmp_slash'),
            'postgres://u@db/tmp_slash')
        self.assertEqual(
            self.url_for('postgres://u@db?dbname=orig', 'tmp_query'),
            'postgres://u@db/tmp_query')
        self.assertEqual(
            self.url_for('postgres://u:p@db:5432?sslmode=disable&dbname=orig', 'tmp_params'),
            'postgres://u:p@db:5432/tmp_params?sslmode=disable')
        self.assertEqual(
            self.url_for('postgres:///orig?dbname=orig', 'tmp_hostless'),
            'postgres:///tmp_hostless')

    def test_db_scripts_use_the_helper_and_assert_the_exact_database(self):
        for name in ('check_pg_upgrade.sh', 'check_pg_races.sh'):
            source = (ROOT / 'scripts' / name).read_text()
            self.assertIn('pg_url_for_database', source, name)
            self.assertIn('current_database()', source, name)


class FinalLandingGate(unittest.TestCase):
    """#5: proposal validation and final landing are distinct, exact-SHA gates."""

    LANES = ('go', 'offline', 'pg', 'pgx', 'sqlc')
    RUN_ID = '12345'
    RUN_ENV = {'GITHUB_RUN_ID': RUN_ID, 'GITHUB_RUN_ATTEMPT': '1'}

    def landing(self, root, *args, env=None):
        script = ROOT / 'scripts' / 'check_landing.py'
        self.assertTrue(script.is_file(), 'scripts/check_landing.py is the landing gate')
        return run(['python3', str(script), '--root', str(root), *args], env=env)

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

    @staticmethod
    def changed(temp, rows=()):
        """The trusted `git diff --name-status base head -- migrations` listing."""
        path = Path(temp) / 'changed-migrations.txt'
        path.write_text(''.join(f'{status}\t{name}\n' for status, name in rows))
        return path

    def lane_identities(self, temp, sha, lanes=None, overrides=None):
        """One independently recorded checkout identity per prerequisite lane."""
        directory = Path(temp) / 'lane-identities'
        directory.mkdir()
        for lane in lanes or self.LANES:
            payload = {
                'lane': lane,
                'event_sha': sha,
                'checked_out_sha': sha,
                'run_id': self.RUN_ENV['GITHUB_RUN_ID'],
                'run_attempt': self.RUN_ENV['GITHUB_RUN_ATTEMPT'],
            }
            payload.update((overrides or {}).get(lane, {}))
            (directory / f'lane-{lane}.json').write_text(json.dumps(payload, sort_keys=True) + '\n')
        return directory

    def nested_lane_identities(self, temp, sha, attempts):
        """One artifact directory per (lane, attempt), as a re-run leaves them.

        A failed attempt still uploads its receipt (the upload step is
        ``if: always()``), so a re-run lane can have receipts from several
        attempts while untouched lanes keep only their first one.
        """
        directory = Path(temp) / 'lane-identities'
        for lane, lane_attempts in attempts.items():
            for attempt in lane_attempts:
                payload = {
                    'lane': lane,
                    'event_sha': sha,
                    'checked_out_sha': sha,
                    'run_id': self.RUN_ENV['GITHUB_RUN_ID'],
                    'run_attempt': str(attempt),
                }
                target = (directory /
                          f"lane-identity-{self.RUN_ENV['GITHUB_RUN_ID']}-{attempt}-{lane}" /
                          f'lane-{lane}.json')
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_text(json.dumps(payload, sort_keys=True) + '\n')
        return directory

    def lane_jobs(self, temp, results=None, sha=None, run_id=None):
        """GitHub's job results for this run, across attempts, as CI fetches them.

        ``results`` maps a lane to ``[(attempt, conclusion), ...]``; the default
        is a clean run where every lane succeeded in attempt 1.
        """
        sha = sha or 'a' * 40
        run_id = self.RUN_ID if run_id is None else run_id
        if results is None:
            results = {lane: [(1, 'success')] for lane in self.LANES}
        rows, job_id = [], 1000
        for lane, lane_results in results.items():
            for attempt, conclusion in lane_results:
                job_id += 1
                rows.append(json.dumps({
                    'id': job_id,
                    'run_id': int(run_id),
                    'run_attempt': attempt,
                    'name': f'foundation-{lane}',
                    'head_sha': sha,
                    'status': 'completed',
                    'conclusion': conclusion,
                }, sort_keys=True))
        path = Path(temp) / 'lane-jobs.jsonl'
        path.write_text(''.join(f'{row}\n' for row in rows))
        return path

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
            lanes = self.lane_identities(temp, 'a' * 40)
            result = self.landing(root, '--mode', 'final', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40,
                                  '--lane-identities', str(lanes), env=self.RUN_ENV)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('FINAL_LANDING_PENDING', combined(result))

    def test_final_mode_rejects_a_numbering_gap(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp, numbered=(1, 3))
            lanes = self.lane_identities(temp, 'a' * 40)
            result = self.landing(root, '--mode', 'final', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40,
                                  '--lane-identities', str(lanes), env=self.RUN_ENV)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('contiguous', combined(result))

    def test_final_mode_rejects_a_synthetic_or_stale_sha(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp)
            lanes = self.lane_identities(temp, 'a' * 40)
            result = self.landing(root, '--mode', 'final', '--sha', 'b' * 40,
                                  '--expected-sha', 'a' * 40,
                                  '--lane-identities', str(lanes), env=self.RUN_ENV)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('FINAL_LANDING_SHA_MISMATCH', combined(result))

    def test_final_mode_writes_the_exact_sha_receipt(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp, numbered=(1, 2, 3))
            lanes = self.lane_identities(temp, 'c' * 40)
            jobs = self.lane_jobs(temp, sha='c' * 40)
            receipt = Path(temp) / 'receipt.json'
            result = self.landing(root, '--mode', 'final', '--sha', 'c' * 40,
                                  '--expected-sha', 'c' * 40,
                                  '--lane-identities', str(lanes),
                                  '--lane-jobs', str(jobs),
                                  '--receipt', str(receipt), env=self.RUN_ENV)
            self.assertEqual(result.returncode, 0, combined(result))
            payload = json.loads(receipt.read_text())
            self.assertEqual(payload['mode'], 'final')
            self.assertEqual(payload['source_sha'], 'c' * 40)
            self.assertEqual(payload['pending'], 0)
            self.assertEqual(payload['numbered'], 3)
            self.assertEqual(sorted(row['lane'] for row in payload['lane_identities']),
                             sorted(self.LANES))

    def test_final_mode_requires_independently_recorded_lane_identities(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp)
            result = self.landing(root, '--mode', 'final', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40, env=self.RUN_ENV)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('FINAL_LANDING_LANE_IDENTITIES', combined(result))

    def test_final_mode_rejects_a_lane_that_tested_a_synthetic_merge(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp)
            lanes = self.lane_identities(
                temp, 'a' * 40, overrides={'offline': {'checked_out_sha': 'd' * 40}})
            jobs = self.lane_jobs(temp)
            result = self.landing(root, '--mode', 'final', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40,
                                  '--lane-identities', str(lanes),
                                  '--lane-jobs', str(jobs), env=self.RUN_ENV)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('FINAL_LANDING_LANE_IDENTITY', combined(result))

    def test_final_mode_rejects_a_missing_lane_identity(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp)
            lanes = self.lane_identities(temp, 'a' * 40, lanes=('go', 'offline', 'pg', 'pgx'))
            jobs = self.lane_jobs(temp)
            result = self.landing(root, '--mode', 'final', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40,
                                  '--lane-identities', str(lanes),
                                  '--lane-jobs', str(jobs), env=self.RUN_ENV)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('sqlc', combined(result))

    def test_final_mode_requires_github_job_results_for_the_receipts(self):
        # A receipt alone cannot prove its attempt succeeded: failed attempts
        # upload one too.  Without GitHub's job results the gate must fail closed.
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp)
            lanes = self.lane_identities(temp, 'a' * 40)
            result = self.landing(root, '--mode', 'final', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40,
                                  '--lane-identities', str(lanes), env=self.RUN_ENV)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('FINAL_LANDING_LANE_JOBS', combined(result))

    def test_final_mode_rejects_job_results_from_another_run(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp)
            lanes = self.lane_identities(temp, 'a' * 40)
            jobs = self.lane_jobs(temp, run_id='99999')
            result = self.landing(root, '--mode', 'final', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40,
                                  '--lane-identities', str(lanes),
                                  '--lane-jobs', str(jobs), env=self.RUN_ENV)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('FINAL_LANDING_LANE_JOBS', combined(result))

    def test_final_mode_rejects_job_results_for_another_sha(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp)
            lanes = self.lane_identities(temp, 'a' * 40)
            jobs = self.lane_jobs(temp, sha='f' * 40)
            result = self.landing(root, '--mode', 'final', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40,
                                  '--lane-identities', str(lanes),
                                  '--lane-jobs', str(jobs), env=self.RUN_ENV)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('FINAL_LANDING_LANE_JOBS', combined(result))

    def test_final_mode_rejects_a_receipt_from_an_unsuccessful_attempt(self):
        # pgx failed; its always-uploaded attempt-1 receipt must not count as a
        # successful lane identity just because the file exists.
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp)
            lanes = self.lane_identities(temp, 'a' * 40)
            jobs = self.lane_jobs(temp, {**{
                lane: [(1, 'success')] for lane in self.LANES if lane != 'pgx'},
                'pgx': [(1, 'failure')]})
            result = self.landing(root, '--mode', 'final', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40,
                                  '--lane-identities', str(lanes),
                                  '--lane-jobs', str(jobs), env=self.RUN_ENV)
            self.assertNotEqual(result.returncode, 0, combined(result))
            output = combined(result)
            self.assertIn('pgx', output)
            self.assertIn('no successful', output)

    def test_final_mode_accepts_mixed_attempt_receipts_after_a_partial_rerun(self):
        # Attempt 1: four lanes succeeded, pgx failed.  Attempt 2 re-runs only
        # pgx, so the four successful lanes keep attempt-1 receipts.  Dropping
        # them because they are not from the current attempt breaks a re-run
        # even though every lane ultimately succeeded.
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp)
            sha = 'e' * 40
            jobs = self.lane_jobs(temp, {**{
                lane: [(1, 'success')] for lane in self.LANES if lane != 'pgx'},
                'pgx': [(1, 'failure'), (2, 'success')]}, sha=sha)
            lanes = self.nested_lane_identities(temp, sha, {
                'go': [1], 'offline': [1], 'pg': [1], 'sqlc': [1], 'pgx': [1, 2]})
            receipt = Path(temp) / 'receipt.json'
            result = self.landing(root, '--mode', 'final', '--sha', sha,
                                  '--expected-sha', sha,
                                  '--lane-identities', str(lanes),
                                  '--lane-jobs', str(jobs),
                                  '--receipt', str(receipt), env=self.RUN_ENV)
            self.assertEqual(result.returncode, 0, combined(result))
            payload = json.loads(receipt.read_text())
            attempts = {row['lane']: row['run_attempt']
                        for row in payload['lane_identities']}
            self.assertEqual(attempts['pgx'], '2')
            self.assertEqual(attempts['go'], '1')
            self.assertEqual(attempts['sqlc'], '1')

    def test_final_mode_prefers_the_latest_successful_attempt(self):
        # Re-running every job gives each lane receipts from every attempt; the
        # latest attempt whose job GitHub reports successful is the evidence.
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp)
            jobs = self.lane_jobs(temp, {lane: [(1, 'success'), (2, 'success')]
                                         for lane in self.LANES})
            lanes = self.nested_lane_identities(
                temp, 'a' * 40, {lane: [1, 2] for lane in self.LANES})
            receipt = Path(temp) / 'receipt.json'
            result = self.landing(root, '--mode', 'final', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40,
                                  '--lane-identities', str(lanes),
                                  '--lane-jobs', str(jobs),
                                  '--receipt', str(receipt), env=self.RUN_ENV)
            self.assertEqual(result.returncode, 0, combined(result))
            payload = json.loads(receipt.read_text())
            self.assertEqual({row['run_attempt'] for row in payload['lane_identities']},
                             {'2'})

    def test_final_mode_replays_shared_execution_identity_vectors(self):
        vectors = json.loads((ROOT / 'tests/fixtures/rerun-attempts.json').read_text())
        for case in vectors['cases']:
            with self.subTest(case=case['name']), tempfile.TemporaryDirectory() as temp:
                root = self.make_repo(temp)
                sha = 'c' * 40
                jobs = self.lane_jobs(temp, sha=sha)
                rows = [json.loads(line) for line in jobs.read_text().splitlines()]
                rows = [job for job in rows if job['name'] != vectors['job_name']]
                source = case.get('jobs', [job for page in case.get('job_pages', []) for job in page])
                rows.extend({**job, 'run_id': int(self.RUN_ID), 'head_sha': sha}
                            for job in source if job['name'] == vectors['job_name'])
                jobs.write_text(json.dumps(rows))
                lanes = self.nested_lane_identities(temp, sha, {
                    **{lane: [1] for lane in self.LANES if lane != 'pg'},
                    'pg': case['evidence_attempts']})
                receipt = Path(temp) / 'receipt.json'
                result = self.landing(root, '--mode', 'final', '--sha', sha,
                                      '--expected-sha', sha, '--lane-identities', str(lanes),
                                      '--lane-jobs', str(jobs), '--receipt', str(receipt),
                                      env=self.RUN_ENV)
                expected = case['expected']
                if 'error' in expected:
                    self.assertNotEqual(result.returncode, 0, combined(result))
                    self.assertIn(expected['error'], combined(result))
                else:
                    self.assertEqual(result.returncode, 0, combined(result))
                    pg = next(row for row in json.loads(receipt.read_text())['lane_identities']
                              if row['lane'] == 'pg')
                    self.assertEqual(int(pg['run_attempt']), expected['attempt'])

    def test_full_rerun_of_every_job_accepts_attempt_two_and_fails_closed(self):
        # Attempt 1 succeeded, then every job was re-run as attempt 2.  Each
        # attempt uploads its own receipt, so the landing download sees both and
        # the newest one whose job GitHub reports successful is the evidence.
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp)
            sha = 'f' * 40
            lanes = self.nested_lane_identities(
                temp, sha, {lane: [1, 2] for lane in self.LANES})
            receipt = Path(temp) / 'receipt.json'
            env = {**self.RUN_ENV, 'GITHUB_RUN_ATTEMPT': '2'}
            jobs = self.lane_jobs(temp, {lane: [(1, 'success'), (2, 'success')]
                                         for lane in self.LANES}, sha=sha)
            result = self.landing(root, '--mode', 'final', '--sha', sha,
                                  '--expected-sha', sha,
                                  '--lane-identities', str(lanes),
                                  '--lane-jobs', str(jobs),
                                  '--receipt', str(receipt), env=env)
            self.assertEqual(result.returncode, 0, combined(result))
            payload = json.loads(receipt.read_text())
            self.assertEqual({row['run_attempt'] for row in payload['lane_identities']},
                             {'2'})
            self.assertEqual(payload['run_attempt'], '2')
            # Fail closed: attempt 1's pgx job failed, and attempt 2 (the
            # re-run) reports pgx successful but its receipt never arrived.  The
            # failed attempt's receipt must not be borrowed.
            failed_jobs = self.lane_jobs(temp, {
                **{lane: [(1, 'success'), (2, 'success')] for lane in self.LANES},
                'pgx': [(1, 'failure'), (2, 'success')]}, sha=sha)
            missing = lanes / f'lane-identity-{self.RUN_ID}-2-pgx' / 'lane-pgx.json'
            self.assertTrue(missing.is_file(), 'fixture must contain the attempt-2 receipt')
            missing.unlink()
            failed = self.landing(root, '--mode', 'final', '--sha', sha,
                                  '--expected-sha', sha,
                                  '--lane-identities', str(lanes),
                                  '--lane-jobs', str(failed_jobs),
                                  '--receipt', Path(temp) / 'failed.json', env=env)
            self.assertNotEqual(failed.returncode, 0, combined(failed))
            self.assertIn('FINAL_LANDING_LANE_IDENTITIES', combined(failed))

    def test_a_newer_successful_attempt_without_its_receipt_is_rejected(self):
        # #426: attempts 1 and 2 both succeeded, but attempt 2's receipt never
        # arrived.  The gate must require the receipt of the latest successful
        # attempt; falling back to attempt 1 contradicts the binding it promises.
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp)
            sha = 'b' * 40
            jobs = self.lane_jobs(temp, {lane: [(1, 'success'), (2, 'success')]
                                         for lane in self.LANES}, sha=sha)
            lanes = self.nested_lane_identities(
                temp, sha, {lane: [1] for lane in self.LANES})
            result = self.landing(root, '--mode', 'final', '--sha', sha,
                                  '--expected-sha', sha,
                                  '--lane-identities', str(lanes),
                                  '--lane-jobs', str(jobs),
                                  '--receipt', Path(temp) / 'receipt.json',
                                  env=self.RUN_ENV)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('latest successful attempt 2', combined(result))

    def test_a_failed_rerun_keeps_the_earlier_successful_receipt(self):
        # #426: a failed-only re-run is reported as a failed job, so the latest
        # successful attempt is still attempt 1 and its receipt stays valid.
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp)
            sha = 'd' * 40
            jobs = self.lane_jobs(temp, {
                **{lane: [(1, 'success')] for lane in self.LANES if lane != 'pg'},
                'pg': [(1, 'success'), (2, 'failure')]}, sha=sha)
            lanes = self.nested_lane_identities(
                temp, sha, {**{lane: [1] for lane in self.LANES if lane != 'pg'},
                            'pg': [1, 2]})
            receipt = Path(temp) / 'receipt.json'
            result = self.landing(root, '--mode', 'final', '--sha', sha,
                                  '--expected-sha', sha,
                                  '--lane-identities', str(lanes),
                                  '--lane-jobs', str(jobs),
                                  '--receipt', str(receipt), env=self.RUN_ENV)
            self.assertEqual(result.returncode, 0, combined(result))
            attempts = {row['lane']: row['run_attempt']
                        for row in json.loads(receipt.read_text())['lane_identities']}
            self.assertEqual(attempts['pg'], '1')

    def test_auto_mode_requires_a_trusted_landing_decision(self):
        # No diff and no trusted policy: auto must not silently become proposal.
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp, pending=('c22a-data-rights',))
            result = self.landing(root, '--mode', 'auto', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40, env=self.RUN_ENV)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('FINAL_LANDING_DECISION_UNKNOWN', combined(result))

    def test_unmarked_landing_change_is_forced_to_final(self):
        # Deleting a proposal and adding a numbered migration is a landing even
        # without the optional [final-landing] marker.
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp, pending=('c20a-fulfillment',))
            changed = self.changed(temp, [
                ('D', 'migrations/pending/c22a-data-rights_data_rights.sql'),
                ('A', 'migrations/003_data_rights.sql'),
            ])
            lanes = self.lane_identities(temp, 'a' * 40)
            result = self.landing(root, '--mode', 'auto', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40,
                                  '--changed-paths', str(changed),
                                  '--lane-identities', str(lanes), env=self.RUN_ENV)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('FINAL_LANDING_PENDING', combined(result))

    def test_explicit_proposal_mode_cannot_override_landing_changes(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp, pending=('c22a-data-rights',))
            changed = self.changed(temp, [
                ('D', 'migrations/pending/c22a-data-rights_data_rights.sql'),
            ])
            result = self.landing(root, '--mode', 'proposal', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40,
                                  '--changed-paths', str(changed), env=self.RUN_ENV)
            self.assertNotEqual(result.returncode, 0, combined(result))
            self.assertIn('FINAL_LANDING_REQUIRED', combined(result))

    def test_a_complete_landing_change_passes_and_records_why(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp, numbered=(1, 2))
            changed = self.changed(temp, [
                ('D', 'migrations/pending/c22a-data-rights_data_rights.sql'),
                ('A', 'migrations/003_data_rights.sql'),
            ])
            lanes = self.lane_identities(temp, 'a' * 40)
            jobs = self.lane_jobs(temp)
            receipt = Path(temp) / 'receipt.json'
            result = self.landing(root, '--mode', 'auto', '--sha', 'a' * 40,
                                  '--expected-sha', 'a' * 40,
                                  '--changed-paths', str(changed),
                                  '--lane-identities', str(lanes),
                                  '--lane-jobs', str(jobs),
                                  '--receipt', str(receipt), env=self.RUN_ENV)
            self.assertEqual(result.returncode, 0, combined(result))
            payload = json.loads(receipt.read_text())
            self.assertEqual(payload['mode'], 'final')
            self.assertTrue(payload['landing_reasons'], payload)

    def test_trusted_policy_selects_final_without_a_marker(self):
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp, numbered=(1, 2))
            changed = self.changed(temp, [])
            lanes = self.lane_identities(temp, 'a' * 40)
            jobs = self.lane_jobs(temp)
            receipt = Path(temp) / 'receipt.json'
            result = self.landing(
                root, '--mode', 'auto', '--sha', 'a' * 40, '--expected-sha', 'a' * 40,
                '--changed-paths', str(changed), '--lane-identities', str(lanes),
                '--lane-jobs', str(jobs), '--receipt', str(receipt),
                env={**self.RUN_ENV, 'FINAL_LANDING_POLICY': '1'})
            self.assertEqual(result.returncode, 0, combined(result))
            self.assertEqual(json.loads(receipt.read_text())['mode'], 'final')

    MARKER = '[final-landing]'

    def marker_decision(self, body):
        """The marker decision the workflow computes from the PR body."""
        script = ROOT / 'scripts' / 'final_landing_marker.py'
        self.assertTrue(
            script.is_file(),
            'scripts/final_landing_marker.py must decide the standalone marker; '
            'a substring match on prose selects final by accident')
        result = run(['python3', str(script)],
                     env={**self.RUN_ENV, 'FINAL_LANDING_BODY': body})
        self.assertEqual(result.returncode, 0, combined(result))
        return result.stdout.strip()

    def test_inline_marker_mention_does_not_select_final(self):
        # #434 (main run 40403380987): #398's squash message mentioned the
        # marker in prose, the push run selected final and failed on pending
        # proposals even though no proposal was removed.
        prose = ('Rework the old decision.\n'
                 f'The squash message of #398 mentioned {self.MARKER} inline, '
                 'so this must stay proposal mode.\n')
        self.assertEqual(self.marker_decision(prose), '0')
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp, pending=('c22a-data-rights',))
            changed = self.changed(temp, [])
            result = self.landing(
                root, '--mode', 'auto', '--sha', 'a' * 40, '--expected-sha', 'a' * 40,
                '--changed-paths', str(changed),
                env={**self.RUN_ENV, 'FINAL_LANDING': self.marker_decision(prose)})
            self.assertEqual(result.returncode, 0, combined(result))
            self.assertIn('PROPOSAL MODE', result.stdout)
            self.assertNotIn('FINAL_LANDING_PENDING', combined(result))

    def test_standalone_marker_line_in_pr_body_still_selects_final(self):
        body = f'Land the data-rights proposal.\n\n{self.MARKER}\n\nNotes follow.\n'
        self.assertEqual(self.marker_decision(body), '1')
        # Only a standalone line counts, never a mention inside a sentence.
        self.assertEqual(self.marker_decision(f'add {self.MARKER} here\n'), '0')
        self.assertEqual(self.marker_decision(f'prefix{self.MARKER}\n'), '0')
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp, numbered=(1, 2))
            changed = self.changed(temp, [])
            lanes = self.lane_identities(temp, 'a' * 40)
            jobs = self.lane_jobs(temp)
            result = self.landing(
                root, '--mode', 'auto', '--sha', 'a' * 40, '--expected-sha', 'a' * 40,
                '--changed-paths', str(changed), '--lane-identities', str(lanes),
                '--lane-jobs', str(jobs),
                env={**self.RUN_ENV, 'FINAL_LANDING': self.marker_decision(body)})
            self.assertEqual(result.returncode, 0, combined(result))
            self.assertIn('FINAL LANDING OK', result.stdout)

    def test_removed_proposal_still_forces_final_without_a_marker(self):
        # Dropping commit-message matching must not weaken the landing gate: a
        # removed proposal selects final from the trusted diff alone.
        with tempfile.TemporaryDirectory() as temp:
            root = self.make_repo(temp, numbered=(1, 2))
            changed = self.changed(temp, [
                ('D', 'migrations/pending/c22a-data-rights_data_rights.sql'),
                ('A', 'migrations/003_data_rights.sql'),
            ])
            lanes = self.lane_identities(temp, 'a' * 40)
            jobs = self.lane_jobs(temp)
            receipt = Path(temp) / 'receipt.json'
            result = self.landing(
                root, '--mode', 'auto', '--sha', 'a' * 40, '--expected-sha', 'a' * 40,
                '--changed-paths', str(changed), '--lane-identities', str(lanes),
                '--lane-jobs', str(jobs), '--receipt', str(receipt),
                env={**self.RUN_ENV, 'FINAL_LANDING': '0'})
            self.assertEqual(result.returncode, 0, combined(result))
            payload = json.loads(receipt.read_text())
            self.assertEqual(payload['mode'], 'final')
            self.assertTrue(payload['landing_reasons'], payload)

    def test_ci_reads_the_marker_only_as_a_standalone_pr_body_line(self):
        jobs = yaml.safe_load((ROOT / '.github/workflows/checks.yml').read_text())['jobs']
        foundation = jobs['foundation']
        steps = foundation['steps']
        marker = [step for step in steps
                  if 'final_landing_marker.py' in step.get('run', '')]
        self.assertEqual(len(marker), 1,
                         'the marker must be computed once from the PR body JSON')
        self.assertIn('github.event.pull_request.body',
                      marker[0]['env'].get('FINAL_LANDING_BODY', ''))
        # Push events have no PR body, and a commit message that merely
        # mentions the marker must never select final landing mode.
        self.assertNotIn('head_commit.message', json.dumps(foundation))
        final = next(step for step in steps
                     if 'check_landing.py --mode auto' in step.get('run', ''))
        self.assertIn('steps.marker.outputs',
                      final['env'].get('FINAL_LANDING', ''))

    def test_ci_binds_landing_mode_to_changes_policy_and_lane_identities(self):
        jobs = yaml.safe_load((ROOT / '.github/workflows/checks.yml').read_text())['jobs']
        foundation = jobs['foundation']
        self.assertEqual(foundation['needs'], ['foundation-lane'])
        runs = '\n'.join(step.get('run', '') for step in foundation['steps'])
        self.assertIn('scripts/check_landing.py --mode auto', runs)
        self.assertIn('--changed-paths', runs)
        self.assertIn('--lane-identities', runs)
        # The observed checkout and the event head stay independent inputs.
        self.assertIn('HEAD_SHA="$(git rev-parse HEAD)"', runs)
        self.assertIn('--sha "$HEAD_SHA"', runs)
        self.assertIn('--expected-sha "$EVENT_HEAD_SHA"', runs)
        # The optional marker may add final validation; it never selects proposal.
        self.assertNotIn('mode=proposal', runs)
        self.assertIn('git diff --name-status', runs)
        self.assertIn('merge-base', runs)
        # Prerequisite lanes test the exact PR head, never the synthetic merge.
        lane_steps = jobs['foundation-lane']['steps']
        checkout = next(s for s in lane_steps
                        if s.get('uses', '').startswith('actions/checkout@'))
        self.assertEqual(checkout['with']['ref'],
                         '${{ github.event.pull_request.head.sha || github.sha }}')
        record = next(s for s in lane_steps if 'checked_out_sha' in s.get('run', ''))
        self.assertIn('rev-parse', record['run'])
        upload = next(s for s in lane_steps
                      if s.get('uses', '').startswith('actions/upload-artifact@')
                      and 'lane-identity' in s.get('with', {}).get('name', ''))
        self.assertIn('${{ github.run_attempt }}', upload['with']['name'])
        self.assertIn('${{ matrix.lane }}', upload['with']['name'])
        download = next(s for s in foundation['steps']
                        if s.get('uses', '').startswith('actions/download-artifact@'))
        pattern = download['with']['pattern']
        # A failed-job re-run keeps earlier attempts: pinning the download to
        # the current attempt drops the receipts of lanes that never re-ran.
        self.assertIn('lane-identity-${{ github.run_id }}-*', pattern)
        self.assertNotIn('github.run_attempt', pattern)
        self.assertNotEqual(download['with'].get('merge-multiple'), True,
                            'per-attempt artifacts must keep distinct directories')
        jobs_step = next(s for s in foundation['steps']
                         if 'jobs?filter=all' in s.get('run', ''))
        self.assertIn('--paginate', jobs_step['run'])
        self.assertIn('.jobs[]', jobs_step['run'])
        # Receipts are checked against GitHub's own per-attempt job results.
        self.assertIn('--lane-jobs', runs)


class RerunAttemptRule(unittest.TestCase):
    """#426/#419: one rule selects the latest successful attempt in both consumers."""

    VECTORS = ROOT / 'tests' / 'fixtures' / 'rerun-attempts.json'

    @staticmethod
    def rule():
        # Imported inside the test so a missing helper reports this control as
        # red instead of breaking collection of every unrelated batch test.
        sys.path.insert(0, str(ROOT / 'scripts'))
        import rerun_attempts
        return rerun_attempts

    def test_shared_vectors_pin_the_latest_successful_attempt_rule(self):
        rule = self.rule()
        vectors = json.loads(self.VECTORS.read_text())
        for case in vectors['cases']:
            with self.subTest(case=case['name']):
                jobs = case.get('jobs')
                if jobs is None:
                    jobs = [job for page in case['job_pages'] for job in page]
                expected = case['expected']
                if 'error' in expected:
                    with self.assertRaises(rule.AttemptError) as caught:
                        rule.select_latest_attempt(
                            rule.successful_attempts(jobs, vectors['job_name']),
                            case['evidence_attempts'])
                    self.assertIn(expected['error'], str(caught.exception))
                else:
                    self.assertEqual(
                        rule.select_latest_attempt(
                            rule.successful_attempts(jobs, vectors['job_name']),
                            case['evidence_attempts']),
                        expected['attempt'])
                    self.assertEqual(rule.latest_successful_attempt(jobs, vectors['job_name']),
                                     expected['attempt'])

    def test_both_consumers_use_the_shared_rule(self):
        landing = (ROOT / 'scripts/check_landing.py').read_text()
        producer = (ROOT / 'scripts/frontend/verify-baseline-artifact.ts').read_text()
        self.assertIn('from rerun_attempts import', landing)
        self.assertIn("from './rerun-attempts'", producer)


class ArtifactRerunSafety(unittest.TestCase):
    """#398: immutable CI artifacts must be safe to re-run at every attempt.

    A full re-run keeps ``github.run_id`` but increments ``github.run_attempt``.
    ``actions/upload-artifact`` refuses to overwrite an existing name, so an
    upload that omits the attempt fails the job on attempt 2 even though the
    validation it reports succeeded.  A failed-job re-run only re-executes the
    failed lanes, leaving earlier attempts' artifacts in place, so the landing
    download must deliberately select every attempt and the gate must still bind
    each receipt to an attempt GitHub reports successful.
    """

    WORKFLOW = ROOT / '.github/workflows/checks.yml'
    RUN_ID = '40318662058'
    UPLOAD = 'actions/upload-artifact@'
    DOWNLOAD = 'actions/download-artifact@'
    RUN_ID_TOKEN = '${{ github.run_id }}'
    ATTEMPT_TOKEN = '${{ github.run_attempt }}'
    LANE_TOKEN = '${{ matrix.lane }}'

    def jobs(self):
        return yaml.safe_load(self.WORKFLOW.read_text())['jobs']

    @classmethod
    def render(cls, template, attempt, lane=None):
        rendered = (template.replace(cls.RUN_ID_TOKEN, cls.RUN_ID)
                    .replace(cls.ATTEMPT_TOKEN, str(attempt)))
        if lane is not None:
            rendered = rendered.replace(cls.LANE_TOKEN, lane)
        return rendered

    def uploads(self):
        found = []
        for job_name, job in self.jobs().items():
            lanes = (job.get('strategy') or {}).get('matrix', {}).get('lane') or [None]
            for step in job.get('steps', []):
                if step.get('uses', '').startswith(self.UPLOAD):
                    found.append((job_name, step['with']['name'], lanes))
        return found

    def test_every_upload_name_is_unique_per_attempt(self):
        uploads = self.uploads()
        self.assertTrue(uploads, 'checks.yml must upload at least the landing receipt')
        by_attempt = {1: set(), 2: set()}
        for job_name, template, lanes in uploads:
            self.assertIn(self.RUN_ID_TOKEN, template, f'{job_name}: {template}')
            self.assertIn(self.ATTEMPT_TOKEN, template,
                          f'{job_name}: artifact {template!r} is re-uploaded unchanged '
                          'on attempt 2 and the immutable-name upload fails')
            # A matrix job uploads once per lane only when the name is lane-scoped;
            # a step gated to one lane (e.g. the offline handoff) uploads once.
            expansions = lanes if self.LANE_TOKEN in template else [None]
            for lane in expansions:
                for attempt in (1, 2):
                    name = self.render(template, attempt, lane)
                    self.assertNotIn(
                        name, by_attempt[attempt],
                        f'{job_name}: duplicate upload name {name!r} in attempt {attempt}')
                    by_attempt[attempt].add(name)
        self.assertFalse(by_attempt[1] & by_attempt[2],
                         'attempt 2 must never reuse an attempt 1 artifact name')

    def test_downloads_select_attempts_deliberately(self):
        downloads = [(job_name, step)
                     for job_name, job in self.jobs().items()
                     for step in job.get('steps', [])
                     if step.get('uses', '').startswith(self.DOWNLOAD)]
        self.assertEqual(len(downloads), 1, downloads)
        job_name, step = downloads[0]
        pattern = self.render(step['with']['pattern'], 2)
        self.assertIn(self.RUN_ID_TOKEN, step['with']['pattern'])
        # Every attempt of this run: a failed-job re-run keeps earlier attempts'
        # receipts, and a full re-run adds the current one.
        self.assertEqual(step['with']['pattern'], f'lane-identity-{self.RUN_ID_TOKEN}-*')
        self.assertNotEqual(step['with'].get('merge-multiple'), True,
                            'per-attempt receipt directories must stay distinct')
        for lane in ('go', 'offline', 'pg', 'pgx', 'sqlc'):
            for attempt in (1, 2):
                name = self.render(f'lane-identity-{self.RUN_ID_TOKEN}-'
                                   f'{self.ATTEMPT_TOKEN}-{self.LANE_TOKEN}', attempt, lane)
                self.assertTrue(fnmatch(name, pattern),
                                f'{name!r} must be downloaded by {pattern!r}')
        for attempt in (1, 2):
            self.assertFalse(
                fnmatch(self.render(f'final-landing-{self.RUN_ID_TOKEN}-'
                                    f'{self.ATTEMPT_TOKEN}', attempt), pattern),
                'the landing receipt must not be read back as a lane identity')
        self.assertIn('--lane-identities', '\n'.join(
            step.get('run', '') for step in self.jobs()['foundation']['steps']))


class PendingMigrationDependency(unittest.TestCase):
    """#225: the scope union must depend on the scopes migration explicitly."""

    STABLE_ID = 'c22a-data-rights'

    def find(self, root):
        helper = ROOT / 'scripts' / 'pending_migrations.py'
        result = run(['python3', str(helper), 'find-stable-id', self.STABLE_ID, str(root)])
        self.assertEqual(result.returncode, 0, combined(result))
        return Path(result.stdout.strip())

    def test_c22a_depends_on_fulfillment_scopes(self):
        # Works in both layouts: still a proposal (pending/) or already landed
        # as a numbered file by CI's ci-normalize or by the integrator.
        text = self.find(ROOT).read_text()
        headers = text.splitlines()
        deps = [line.split(':', 1)[1].strip() for line in headers if line.startswith('-- depends-on:')]
        self.assertIn('c20a-fulfillment-scopes', deps)
        self.assertNotIn('c20a-fulfillment', deps)

    def test_lookup_covers_pending_and_numbered_layouts(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            pending_dir = root / 'migrations' / 'pending'
            pending_dir.mkdir(parents=True)
            (root / 'migrations' / '001_init.sql').write_text('-- stable-id: 001-init\nSELECT 1;\n')
            pending = pending_dir / f'{self.STABLE_ID}_data_rights.sql'
            pending.write_text(f'-- stable-id: {self.STABLE_ID}\nSELECT 1;\n')
            self.assertEqual(self.find(root), pending.resolve())
            landed = root / 'migrations' / '002_data_rights.sql'
            pending.rename(landed)
            self.assertEqual(self.find(root), landed.resolve())

    def test_missing_stable_id_fails_closed(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / 'migrations').mkdir()
            (root / 'migrations' / '001_init.sql').write_text('-- stable-id: 001-init\nSELECT 1;\n')
            helper = ROOT / 'scripts' / 'pending_migrations.py'
            result = run(['python3', str(helper), 'find-stable-id', self.STABLE_ID, str(root)])
            self.assertNotEqual(result.returncode, 0, combined(result))


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
