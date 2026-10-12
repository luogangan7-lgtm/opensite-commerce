#!/usr/bin/env python3
"""Offline CLI flow test: verifies expected PG files are submitted to psql.

Depends on check_pg.sh/migrate.sh and stdlib; called by offline CI.
Invariant: every SQL gate is submitted in order and SQL errors stop the runner.
This is NOT a PostgreSQL execution test. Its fake psql never parses SQL.
"""
from pathlib import Path
import json
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
    'db_security_hardening.sql',
    'db_role_matrix.sql',
    'db_connection_reuse.sql',
    'db_security_services.sql',
    'db_publish_isolation.sql',
    'db_sealed_media.sql',
    'db_reliability.sql',
    'db_inventory.sql',
    'db_pricing.sql',
    'db_shipping.sql',
    'db_identity_auth.sql',
    'db_identity_login.sql',
]
assert 'tests/db_identity_auth.sql' in (root / 'scripts/check_pg.sh').read_text(), 'identity auth SQL gate missing from runner manifest'
# Coverage guards only: the real-PG runners execute these bootstrap cases.
assert 'BOOTSTRAP_RACE=' in (root / 'scripts/check_pg_races.sh').read_text(), 'first-owner concurrency case missing'
assert 'BOOTSTRAP_UPGRADE=' in (root / 'scripts/check_pg_upgrade.sh').read_text(), 'bootstrap populated-upgrade cases missing'
assert 'LOGIN_ROTATION_RACE=' in (root / 'scripts/check_pg_races.sh').read_text(), 'login conditional rotation concurrency missing'
assert 'LOGIN_UPGRADE=' in (root / 'scripts/check_pg_upgrade.sh').read_text(), 'session last_seen populated upgrade missing'
# Exercise the production-query extraction without connecting to PG. The real
# race still has to establish row-lock ordering and exactly one valid successor.
race_source = (root / 'scripts/check_pg_races.sh').read_text()
login_race = race_source.split('# C-02b:', 1)[1].split('# First-owner is installation-wide:', 1)[0]
assert '2040-' not in login_race, 'login rotation fixture must use current database time'
assert "clock_timestamp()-interval '10 minutes'" in login_race, 'old session must be valid against DB execution clock'
extractor = race_source.split("<<'PYSQL'\n", 1)[1].split('\nPYSQL', 1)[0]
probe = subprocess.run(['python3', '-', '019abcd0-1111-7222-8333-000000000001', 'a'*64, 'b'*64],
                       input=extractor, cwd=root, capture_output=True, text=True)
assert probe.returncode == 0, probe.stderr
assert '2040-' not in probe.stdout, 'new sessions must use current DB time'
assert "(clock_timestamp()+interval '1 hour')::timestamptz" in probe.stdout, 'expiry expression must be grouped before sqlc parameter cast'
assert probe.stdout.count('\\gset') == 1 and 'sqlc.' not in probe.stdout
assert probe.stdout.index('UPDATE app.auth_sessions') < probe.stdout.index('INSERT INTO app.auth_sessions')
assert 's.user_id=' in probe.stdout and 's.revoked_at IS NULL' in probe.stdout
assert 's.expires_at>' in probe.stdout and 's.last_seen_at>' in probe.stdout
n_migrations = len(list((root / 'migrations').glob('[0-9][0-9][0-9]_*.sql')))
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
  if [[ "$payload" == *"MODULE_TABLE_OWNERSHIP"* ]]; then
    printf '%s\n' "$FAKE_TABLES_JSON"
  fi
fi
''')
    fake.chmod(0o755)
    registry = json.loads((root / 'modules/registry.yaml').read_text())
    tables = sorted(table for module in registry['modules'] if module['status'] == 'implemented'
                    for table in module['owns_tables'])
    env = dict(os.environ, DATABASE_URL='postgresql://fake/opensite',
               FAKE_TABLES_JSON=json.dumps(tables),
               FAKE_PSQL_LOG=str(log), PATH=str(binpath)+os.pathsep+os.environ['PATH'])
    command = ['bash', str(root / 'scripts' / 'check_pg.sh')]
    r = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
    assert r.returncode == 0, (r.stdout, r.stderr)
    for name in expected:
        assert f'RUN tests/{name}' in r.stdout, name
    contents = log.read_text()
    assert contents.count('SELECT pg_advisory_xact_lock') == 2 * (n_migrations + 1), '2 runs x (migrations + 1 history gate)'
    assert contents.count('===SQL===') == 2 * (n_migrations + 1) + len(expected) + 1, '2 x migration checks + test files + catalog audit'
    assert contents.count('MODULE_TABLE_OWNERSHIP') == 1
    assert 'PASS: module ownership' in r.stdout
    assert 'boundary-roles' in contents
    assert 'release-FK' in contents
    assert 'smoke-A' in contents

    # A failed database test must stop the acceptance script with a nonzero code.
    env['FAKE_FAIL_ON'] = 'boundary-roles'
    failed = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
    assert failed.returncode != 0, (failed.stdout, failed.stderr)
    assert 'RUN tests/db_release_integrity.sql' not in failed.stdout
    # The audit itself must propagate connection/catalog errors and owner drift.
    env['FAKE_FAIL_ON'] = 'MODULE_TABLE_OWNERSHIP'
    failed = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
    assert failed.returncode != 0 and 'MODULE_DATABASE_ERROR' in failed.stderr
    env.pop('FAKE_FAIL_ON')
    env['FAKE_TABLES_JSON'] = json.dumps(tables + ['unregistered_table'])
    failed = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
    assert failed.returncode != 0 and 'MODULE_UNOWNED_TABLE' in failed.stderr
    env['FAKE_TABLES_JSON'] = json.dumps(tables)
    env['FAKE_FAIL_ON'] = 'SHIPPING_OVERLAP_ACCEPTED'
    failed = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
    assert failed.returncode == 31, (failed.stdout, failed.stderr)
    assert 'RUN tests/db_shipping.sql' in failed.stdout

    env['FAKE_FAIL_ON'] = 'IDENTITY_GATE'
    failed = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
    assert failed.returncode == 31, (failed.stdout, failed.stderr)
    assert 'RUN tests/db_identity_auth.sql' in failed.stdout
    env['FAKE_FAIL_ON'] = 'LOGIN_GATE'
    failed = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
    assert failed.returncode == 31, (failed.stdout, failed.stderr)
    assert 'RUN tests/db_identity_login.sql' in failed.stdout
print(f'PASS: check_pg submits all {len(expected)} real-PG test files, repeats {n_migrations} migrations, propagates failures (mock psql; NO SQL executed)')
