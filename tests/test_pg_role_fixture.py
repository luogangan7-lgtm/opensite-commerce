#!/usr/bin/env python3
"""Keep cluster role SQL in the shared, locked fixture helper (offline lane).

The scan itself is AST-based: scripts/rolegate parses every *_test.go, folds
string literals and named string constants, and rejects cluster role writes.
These tests drive that program over the repository and over testdata fixtures.
"""
from pathlib import Path
import os
import re
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[1]
TESTDATA = ROOT / 'scripts' / 'rolegate' / 'testdata'
# Only the helper package that owns the common advisory lock may write roles.
ALLOWED = 'internal/platform/pgxtest/'

FAIL_FIXTURES = (
    'hex_space_test.go',      # "CREATE\x20ROLE x"
    'octal_space_test.go',    # "CREATE\040ROLE x"
    'unicode_space_test.go',  # "CREATE\u0020ROLE x"
    'plain_space_test.go',    # "CREATE ROLE x"
    'concat_test.go',         # "CREATE " + "ROLE " + name
    'const_test.go',          # const q = "GRANT x TO "
    'const_cross_file_test.go',  # const sharedRoleSQL in another file of the package
    'func_const_test.go',     # const prefix = "CREATE " declared inside a function body
    'raw_string_test.go',     # `CREATE ROLE x`
    'split_keyword_test.go',  # raw newline/tab between keywords
    'escape_split_test.go',   # "REVOKE x\nFROM y"
    'const_omitted_list_test.go',  # omitted two-name list broadcast the last expression
    'const_omitted_three_test.go',  # omitted three-name list binds by index
    'const_omitted_twice_test.go',  # two consecutive omitted lists repeat the full list
    'const_omitted_cross_file_test.go',  # omitted list declared in another package file
    'func_const_omitted_list_test.go',   # omitted list inside a function body
)


def run_gate(root, *args):
    env = dict(os.environ, GOPROXY='off', GOSUMDB='off')
    return subprocess.run(
        ['go', 'run', './scripts/rolegate', '-root', str(root), *args],
        cwd=ROOT, env=env, capture_output=True, text=True, timeout=180)


def output(result):
    return result.stdout + result.stderr


class RoleFixtureGate(unittest.TestCase):
    def test_repo_has_no_raw_role_sql(self):
        # testdata/ is skipped by the scanner, so the deliberately bad fixtures
        # below can live in-tree without making this assertion fail.
        result = run_gate(ROOT, '-allow', ALLOWED)
        self.assertEqual(result.returncode, 0, output(result))

    def test_gate_flags_hidden_and_folded_role_sql(self):
        result = run_gate(TESTDATA / 'fail', '-allow', '')
        self.assertNotEqual(result.returncode, 0, output(result))
        for name in FAIL_FIXTURES:
            self.assertRegex(
                result.stdout, rf'(?m)^{re.escape(name)}:\d+: cluster role SQL',
                f'{name} was not reported:\n{output(result)}')

    def test_gate_passes_helper_calls_and_object_privileges(self):
        result = run_gate(TESTDATA / 'pass', '-allow', '')
        self.assertEqual(result.returncode, 0, output(result))

    def test_allowlist_keeps_pgxtest_semantics(self):
        allowed = run_gate(TESTDATA / 'allowlist', '-allow', ALLOWED)
        self.assertEqual(allowed.returncode, 0, output(allowed))
        # The same fixture fails once the allowlist is removed, proving the pass
        # above comes from the allowlist rather than a missed literal.
        blocked = run_gate(TESTDATA / 'allowlist', '-allow', '')
        self.assertNotEqual(blocked.returncode, 0, output(blocked))
        self.assertRegex(
            blocked.stdout,
            r'(?m)^internal/platform/pgxtest/role_test\.go:\d+: cluster role SQL',
            output(blocked))


if __name__ == '__main__':
    unittest.main()
