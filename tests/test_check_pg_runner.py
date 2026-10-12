#!/usr/bin/env python3
"""Offline CLI flow test: verifies expected PG files are submitted to psql.

This is NOT a PostgreSQL execution test. Its fake psql never parses SQL.
"""
from pathlib import Path
import os
import subprocess
import tempfile

root = Path(__file__).resolve().parents[1]
expected = [
    'db_security_audit.sql',
    'db_smoke.sql',
    'db_isolation.sql',
    'db_role_boundaries.sql',
    'db_release_integrity.sql',
]
for name in expected:
    assert (root / 'tests' / name).is_file(), name

with tempfile.TemporaryDirectory() as tmp:
    binpath = Path(tmp)
    log = binpath / 'calls.log'
    fake = binpath / 'psql'
    fake.write_text(r'''#!/bin/bash
payload="$(cat)"
if [[ "$payload" == *"SHOW server_version_num"* ]]; then
  echo '180002'
else
  printf '===SQL===\n%s\n' "$payload" >> "$FAKE_PSQL_LOG"
  if [[ -n "${FAKE_FAIL_ON:-}" && "$payload" == *"$FAKE_FAIL_ON"* ]]; then
    exit 31
  fi
fi
''')
    fake.chmod(0o755)
    env = dict(os.environ, DATABASE_URL='postgresql://fake/opensite',
               FAKE_PSQL_LOG=str(log), PATH=str(binpath)+os.pathsep+os.environ['PATH'])
    command = ['bash', str(root / 'scripts' / 'check_pg.sh')]
    r = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
    assert r.returncode == 0, (r.stdout, r.stderr)
    for name in expected:
        assert f'RUN tests/{name}' in r.stdout, name
    contents = log.read_text()
    assert contents.count('SELECT pg_advisory_xact_lock') == 20, '2 runs x (9 migrations + 1 history gate)'
    assert contents.count('===SQL===') == 25, '2 x 10 migration checks + 5 actual test files'
    assert 'boundary-roles' in contents
    assert 'release-FK' in contents
    assert 'smoke-A' in contents

    # A failed database test must stop the acceptance script with a nonzero code.
    env['FAKE_FAIL_ON'] = 'boundary-roles'
    failed = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
    assert failed.returncode != 0, (failed.stdout, failed.stderr)
    assert 'RUN tests/db_release_integrity.sql' not in failed.stdout
print('PASS: check_pg submits all 5 real-PG test files, repeats 9 migrations, propagates failures (mock psql; NO SQL executed)')
