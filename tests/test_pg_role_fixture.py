#!/usr/bin/env python3
"""Keep cluster role SQL in the shared, locked fixture helper (offline lane)."""
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[1]
# GRANT object privileges contain ON; only role memberships are cluster global.
ROLE_SQL = re.compile(r'\b(?:CREATE\s+ROLE\b|ALTER\s+ROLE\b|DROP\s+ROLE\b|GRANT\b(?![^;"`\n]*\bON\b)\s|(?:INSERT\s+INTO|UPDATE|DELETE\s+FROM)\s+(?:pg_catalog\.)?pg_auth_members\b)', re.I)
LITERAL = re.compile(r'`[^`]*`|"(?:\\.|[^"\\])*"|//[^\n]*|/\*.*?\*/', re.S)


def role_sql(text):
    # Keep string literals, including concatenated fragments; discard comments.
    return [m.group() for m in LITERAL.finditer(text)
            if not m.group().startswith(('//', '/*')) and ROLE_SQL.search(m.group())]


class RoleFixtureGate(unittest.TestCase):
    def test_no_raw_role_sql(self):
        offenders = [str(p.relative_to(ROOT)) for p in ROOT.rglob('*_test.go')
                     if role_sql(p.read_text())]
        self.assertEqual(offenders, [], 'cluster role writes must use pgxtest.NewRole')

    def test_matcher_bites(self):
        for sql in ('CREATE ROLE ', 'ALTER ROLE ', 'DROP ROLE ', 'GRANT app_auth TO ',
                    'GRANT " + role + " TO ', 'UPDATE pg_auth_members SET x=1'):
            self.assertTrue(role_sql('`' + sql + '`'), sql)
        self.assertFalse(role_sql('`GRANT SELECT ON app.users TO app_runtime`'))
        self.assertFalse(role_sql('// CREATE ROLE ignored'))


if __name__ == '__main__':
    unittest.main()
