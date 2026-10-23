#!/usr/bin/env python3
"""Keep cluster role SQL in the shared, locked fixture helper (offline lane)."""
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[1]
# Only the helper package that owns the common advisory lock may write roles.
ALLOWED = ('internal/platform/pgxtest/',)
# GRANT object privileges contain ON; only role memberships are cluster global.
# Matching runs on whitespace-normalized text, so keywords may be split by
# whitespace inside one literal or across concatenated chunks.
ROLE_SQL = re.compile(r'\b(?:CREATE\s+ROLE\b|ALTER\s+ROLE\b|DROP\s+ROLE\b|GRANT\s+(?![^;\n]*\bON\b)(?:[^;\n]*\bTO\b|$)|REVOKE\s+(?![^;\n]*\bON\b)(?:[^;\n]*\bFROM\b|$)|(?:INSERT\s+INTO|UPDATE|DELETE\s+FROM)\s+(?:pg_catalog\.)?pg_auth_members\b)', re.I)
ESCAPED_SPACE = {'n': ' ', 'r': ' ', 't': ' '}


def read_string(src, start):
    """Return (decoded literal body, index after the closing quote)."""
    quote, i = src[start], start + 1
    if quote == '`':
        end = src.find('`', i)
        return (src[i:] if end < 0 else src[i:end]), (len(src) if end < 0 else end + 1)
    out = []
    while i < len(src):
        if src[i] == '\\' and i + 1 < len(src):
            escaped = src[i + 1]
            if escaped == '\n':  # line continuation contributes nothing
                i += 2
                continue
            out.append(ESCAPED_SPACE.get(escaped, escaped))
            i += 2
            continue
        if src[i] == '"':
            return ''.join(out), i + 1
        out.append(src[i])
        i += 1
    return ''.join(out), i


def string_values(src):
    """Yield every maximal `+`-concatenated Go string expression, decoded.

    Comments are discarded, so literals quoted inside a comment never match, and
    a split like `"CREATE " + /* lock */ "ROLE "` is still joined.
    """
    values, current, pending_plus = [], None, False
    i, n = 0, len(src)
    while i < n:
        c = src[i]
        if c in ' \t\r\n':
            i += 1
        elif src.startswith('//', i):
            end = src.find('\n', i)
            i = n if end < 0 else end + 1
        elif src.startswith('/*', i):
            end = src.find('*/', i + 2)
            i = n if end < 0 else end + 2
        elif c in '`"':
            body, i = read_string(src, i)
            if pending_plus and current is not None:
                current += body
            else:
                if current is not None:
                    values.append(current)
                current = body
            pending_plus = False
        elif c == "'":  # rune literal such as '+' never joins strings
            i += 1
            while i < n and src[i] != "'":
                i += 2 if src[i] == '\\' else 1
            i += 1
            if current is not None:
                values.append(current)
                current, pending_plus = None, False
        elif c == '+' and i + 1 < n and src[i + 1] in '+=':
            if current is not None:
                values.append(current)
                current, pending_plus = None, False
            i += 1
        elif c == '+':
            if current is not None:
                pending_plus = True
            i += 1
        else:
            if current is not None:
                values.append(current)
                current, pending_plus = None, False
            i += 1
    if current is not None:
        values.append(current)
    return values


def role_sql(text):
    """Return concatenated literal chunks that contain cluster role SQL."""
    return [value for value in string_values(text)
            if ROLE_SQL.search(re.sub(r'\s+', ' ', value))]


class RoleFixtureGate(unittest.TestCase):
    def test_no_raw_role_sql(self):
        offenders = []
        for path in ROOT.rglob('*_test.go'):
            rel = path.relative_to(ROOT).as_posix()
            if rel.startswith(ALLOWED):
                continue
            if role_sql(path.read_text()):
                offenders.append(rel)
        self.assertEqual(offenders, [], 'cluster role writes must use pgxtest.NewRole')

    def test_matcher_bites(self):
        for sql in ('CREATE ROLE ', 'ALTER ROLE ', 'DROP ROLE ', 'GRANT app_auth TO ', 'GRANT ', 'REVOKE app_auth FROM ',
                    'GRANT " + role + " TO ', 'UPDATE pg_auth_members SET x=1'):
            self.assertTrue(role_sql('`' + sql + '`'), sql)
        self.assertFalse(role_sql('`GRANT SELECT ON app.users TO app_runtime`'))
        self.assertFalse(role_sql('// CREATE ROLE ignored'))
        self.assertFalse(role_sql('"preview grant setup status=%d"'))

    def test_split_literals_are_joined(self):
        self.assertTrue(role_sql('sql := "CREATE " + "ROLE " + name'))
        self.assertTrue(role_sql('sql := "GRANT " + "app_auth " + "TO " + name'))
        self.assertTrue(role_sql('sql := `DROP ` + /* locked */ "ROLE " + name'))
        self.assertTrue(role_sql('sql := "ALTER " +\n\t"ROLE " + name'))

    def test_split_keywords_bite(self):
        self.assertTrue(role_sql('sql := `GRANT app_auth\nTO app_user`'))
        self.assertTrue(role_sql('sql := "GRANT app_auth\\nTO app_user"'))
        self.assertTrue(role_sql('sql := `CREATE\tROLE\n app_login`'))

    def test_helper_calls_pass(self):
        for call in ('pgxtest.NewRole(t, ctx, dsn, "app_login", password, false, "app_runtime")',
                     'pgxtest.NewRole(t, ctx, dsn, "app_login", password, true, "app_auth")',
                     'role.Grant(t, ctx, "app_auth")',
                     'role.Alter(t, ctx, "NOINHERIT")'):
            self.assertFalse(role_sql(call), call)
        self.assertFalse(role_sql('// pgxtest.NewRole("CREATE ROLE x")'))


if __name__ == '__main__':
    unittest.main()
