#!/usr/bin/env python3
"""Offline CLI flow test: verifies expected PG files are submitted to psql.

This is NOT a PostgreSQL execution test. Its fake psql never parses SQL.
"""
from pathlib import Path
import json
import os
import re
import subprocess
import tempfile

root = Path(__file__).resolve().parents[1]
# Exercise the actual race helper independently of PostgreSQL. Presence alone
# cannot prove that a participant finishes or its failed exit reaches the gate.
race_source = (root / 'scripts/check_pg_races.sh').read_text()
helper_match = re.search(r'^await_job\(\) \{\n.*?^\}\n', race_source, re.MULTILINE | re.DOTALL)
assert helper_match, 'Customer independent-tenant race requires await_job helper'
helper = helper_match.group()
with tempfile.TemporaryDirectory() as job_tmp:
    def check_job(body, implementation=helper):
        result = subprocess.run(['bash', '-c', implementation + body],
                                cwd=job_tmp, capture_output=True, text=True, timeout=5)
        assert result.returncode == 0, (result.stdout, result.stderr)

    check_job(r'''
RACE_PREFIX=offline_; fail=0
(sleep 0.2; touch completed) & pid=$!
kill -0 "$pid" || exit 10
await_job completed "$pid" || exit 11
[[ -f completed && $fail == 0 ]] || exit 12
''')
    check_job(r'''
RACE_PREFIX=offline_; fail=0
(sleep 0.1; exit 7) & pid=$!
kill -0 "$pid" || exit 20
await_job rejected "$pid"; status=$?
[[ $status == 7 ]] || exit 21
''')
    assert helper.count('SECONDS + 15') == 1, 'await_job must retain bounded production deadline'
    check_job(r'''
RACE_PREFIX=offline_; fail=0
sleep 2 & pid=$!
trap 'kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true' EXIT
kill -0 "$pid" || exit 30
await_job timeout "$pid"; status=$?
[[ $status == 1 && $fail == 1 ]] || exit 31
''', helper.replace('SECONDS + 15', 'SECONDS + 0'))
print('PASS: actual await_job waits, propagates child failure, and marks bounded timeout')

# Run the real limits upgrade fragment: its populated witness must precede
# successors, while its final whole-schema matrix requires every migration.
upgrade_source = (root / 'scripts/check_pg_upgrade.sh').read_text()
limits_start = upgrade_source.index('# INT25:')
limits_end = upgrade_source.index('[[ $fail -eq 0 ]]', limits_start)
limits_case = upgrade_source[limits_start:limits_end]
with tempfile.TemporaryDirectory() as limits_tmp:
    limits_env = dict(os.environ, APPLIED_LOG=str(Path(limits_tmp) / 'applied'),
                      WITNESS_LOG=str(Path(limits_tmp) / 'witness'))
    limits_mock = r'''set -euo pipefail
fail=0
: > "$APPLIED_LOG"
admin() { :; }
river_db() { :; }
pass() { :; }
bad() { echo "FAIL: $*" >&2; fail=1; }
apply_forward() {
 if [[ $2 > ${limits_files[0]} && ! -f "$WITNESS_LOG" ]]; then
  echo 'LIMITS_SUCCESSOR_BEFORE_WITNESS' >&2; return 43
 fi
 printf '%s\n' "$2" >> "$APPLIED_LOG"
}
db() {
 local payload expected actual
 if [[ ${2:-} == -f ]]; then
  if [[ $3 == tests/db_role_matrix.sql ]]; then
   expected=$(printf '%s\n' migrations/[0-9][0-9][0-9]_*.sql)
   actual=$(cat "$APPLIED_LOG")
   [[ $actual == "$expected" ]] || {
    echo 'LIMITS_UPGRADE_MISSING_SUCCESSOR: final matrix requires all migrations' >&2
    return 44
   }
  fi
 else
  payload=$(cat)
  if [[ $payload == *LIMITS_UPGRADE=* ]]; then
   : > "$WITNESS_LOG"
   echo 'LIMITS_UPGRADE=4|2|2|10'
  fi
 fi
}
'''
    limits_result = subprocess.run(['bash', '-c', limits_mock + limits_case],
                                   cwd=root, env=limits_env, capture_output=True,
                                   text=True, timeout=5)
    assert limits_result.returncode == 0, (limits_result.stdout, limits_result.stderr)
print('PASS: real limits upgrade fragment applies all migrations before final matrix, witness before successors (mock functions; NO SQL executed)')

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
    'db_tax.sql',
    'db_customers.sql',
    'db_ledger.sql',
    'db_catalog_api.sql',
    'db_orders.sql',
    'db_orders_guards.sql',
    'db_queue.sql',
    'db_reliability_limits.sql',
    'db_cart.sql',
    'db_storefront.sql',
    'db_ops_metrics.sql',
    'db_notifications.sql',
    'db_quotes.sql',
    'db_identity_auth.sql',
    'db_identity_login.sql',
    'db_cms_publish.sql',
    'db_checkout.sql',
    'db_customer_auth.sql',
    'db_payments.sql',
]
# Coverage guards only: real-PG runners execute the locking/upgrade contracts.
races = (root / 'scripts/check_pg_races.sh').read_text()
upgrade = (root / 'scripts/check_pg_upgrade.sh').read_text()

def payments_runner_contract(source):
    section = source[source.index('# C-19a gates93/94:'):source.index('if [[ $fail -ne 0 ]]; then')]
    assert 'gate_close 93' in section and 'gate_close 94' in section
    assert 'await payment_inbox_second "${ROWLOCK[@]}"' in section
    assert 'await payment_stream_second advisory' in section
    assert "app.lock_payment_stream('$ACCOUNT','intent-one')" in section
    assert "app.lock_payment_stream('$ACCOUNT','intent-two')" in section
    assert 'result payment_stream_other C19_STREAM_OTHER' in section
    assert 'C19_INBOX_INSERTED' in section and "[[ $got == '1|true' ]]" in section

payments_runner_contract(races)
for broken in [races.replace('await payment_inbox_second "${ROWLOCK[@]}"', 'true'),
               races.replace('await payment_stream_second advisory', 'true'),
               races.replace('result payment_stream_other C19_STREAM_OTHER', 'true')]:
    try:
        payments_runner_contract(broken)
    except (AssertionError, ValueError):
        pass
    else:
        raise AssertionError('payments synchronization mutation escaped the runner guard')

def identity_runner_contract(races, upgrade):
    assert 'BOOTSTRAP_RACE=' in races, 'first-owner concurrency case missing'
    assert 'BOOTSTRAP_UPGRADE=' in upgrade, 'bootstrap populated-upgrade cases missing'
    auth = races[races.index('# C-02a: global auth'):races.index('# First-owner is installation-wide:')]
    assert 'pg_blocking_pids(second.pid)' in auth, 'auth race requires an observed blocker'
    assert 'await auth_second advisory' in auth, 'auth contender must be parked before release'
    assert '::timestamptz' not in auth, 'caller clock must not enter auth failure API'
    assert not any(line.lstrip().startswith('sleep ') or 'pg_sleep(' in line for line in auth.splitlines() if not line.lstrip().startswith('#')), 'auth ordering must use observed locks'
    assert "expected 2|2" in auth, 'account and account+IP counts must both survive'
    assert "result prune_skip 'pruned=0|0'" in auth, 'cleanup must skip held stale rows'
    assert "result prune_live 'pruned=0|0'" in auth, 'cleanup must preserve renewed rows'
    boot = races[races.index('# First-owner is installation-wide:'):]
    assert 'for f in migrations/[0-9][0-9][0-9]_*.sql; do' in boot, 'bootstrap must migrate all modules'
    assert 'if [[ $f == *_queue_authorization.sql ]]' in boot and './scripts/migrate_river.sh' in boot, 'bootstrap must install River before queue authorization'
    assert 'pg_blocking_pids(second.pid)' in boot, 'bootstrap contender must block on owner lock'
    identity = upgrade[upgrade.index('# C-02a: discover'):upgrade.index('# C-02b: start')]
    assert 'identity_files=(migrations/[0-9][0-9][0-9]_identity_auth.sql)' in identity
    assert '[[ $f < ${identity_files[0]} ]] || break' in identity, 'all predecessors must be applied'
    assert '[[ $f > ${identity_files[0]} ]] || continue' in identity, 'all successors must be applied'
    assert identity.count('apply_forward "$DB" "$f"') == 2, 'identity predecessors/successors must retain River-aware apply_forward'
    assert 'IDENTITY_UPGRADE=4|4|4' in identity, 'legacy whole-row witnesses must remain exact'

identity_runner_contract(races, upgrade)
def login_runner_contract(races, upgrade):
    rotation = races[races.index('# C-02b: execute'):]
    assert "source = Path('queries/identity/login.sql').read_text()" in rotation
    assert "query('LoginRotateOldSession'" in rotation and "query('LoginInsertSession'" in rotation
    assert 'RETURNING s.id AS rotated' in rotation and '\\gset' in rotation, 'zero-row rotation must abort before successor INSERT'
    assert 'pg_blocking_pids(second.pid)' in rotation, 'login rotation requires an observed blocker'
    assert "expect login_rotate_second 'no rows returned for'" in rotation
    assert 'LOGIN_ROTATION_RACE=1|1|1|0' in rotation, 'exactly one successor must remain'
    login = upgrade[upgrade.index('# C-02b: start'):]
    assert 'login_files=(migrations/[0-9][0-9][0-9]_identity_login.sql)' in login
    assert '[[ $f < ${login_files[0]} ]] || break' in login
    assert '[[ $f > ${login_files[0]} ]] || continue' in login
    assert login.count('apply_forward "$DB" "$f"') == 2, 'login must retain River-aware predecessor/successor application'
    assert 'LOGIN_UPGRADE=3|3|3|true|true|false' in login

login_runner_contract(races, upgrade)
for bad_races, bad_upgrade in [
    (races.replace('RETURNING s.id AS rotated', 'REMOVED'), upgrade),
    (races.replace('LOGIN_ROTATION_RACE=1|1|1|0', 'LOGIN_ROTATION_RACE=1|2|1|1'), upgrade),
    (races, upgrade.replace('apply_forward "$DB" "$f"', 'true')),
    (races, upgrade.replace('[[ $f < ${login_files[0]} ]] || break', 'true')),
]:
    try:
        login_runner_contract(bad_races, bad_upgrade)
    except (AssertionError, ValueError):
        pass
    else:
        raise AssertionError('login runner mutation escaped the guard')
# Mutation witnesses: the guard must turn red when synchronization, the clock
# boundary, predecessor discovery or the bootstrap case is removed.
for bad_races, bad_upgrade in [
    (races.replace('pg_blocking_pids(second.pid)', 'ARRAY[]::integer[]'), upgrade),
    (races.replace('::inet);', "::inet,'2030-01-01Z'::timestamptz);"), upgrade),
    (races.replace('BOOTSTRAP_RACE=', 'REMOVED='), upgrade),
    (races.replace("result prune_skip 'pruned=0|0'", "result prune_skip 'pruned=1|1'"), upgrade),
    (races.replace('if [[ $f == *_queue_authorization.sql ]]', 'if false'), upgrade),
    (races, upgrade.replace('apply_forward "$DB" "$f"', 'true')),
    (races, upgrade.replace('[[ $f < ${identity_files[0]} ]] || break', 'true')),
]:
    try:
        identity_runner_contract(bad_races, bad_upgrade)
    except (AssertionError, ValueError):
        pass
    else:
        raise AssertionError('identity runner mutation escaped the guard')
n_migrations = len(list((root / 'migrations').glob('[0-9][0-9][0-9]_*.sql')))
for name in expected:
    assert (root / 'tests' / name).is_file(), name

with tempfile.TemporaryDirectory() as tmp:
    binpath = Path(tmp)
    log = binpath / 'calls.log'
    flow = binpath / 'flow.log'
    fake = binpath / 'psql'
    fake.write_text(r'''#!/bin/bash
payload="$(cat)"
if [[ "$payload" == *"SHOW server_version_num"* ]]; then
  echo '180002'
else
  if [[ "$payload" == *"MODULE_TABLE_OWNERSHIP"* ]]; then
    echo CATALOG >> "$FAKE_FLOW_LOG"
  else
    echo SQL >> "$FAKE_FLOW_LOG"
  fi
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
    fake_go = binpath / 'go'
    fake_go.write_text(r'''#!/bin/bash
[[ "$1" == run && "$2" == ./cmd/river-migrate ]] || { echo 'unexpected Go command' >&2; exit 35; }
kind=INSTALL
[[ " $* " != *" --verify "* ]] || kind=VERIFY
echo "$kind" >> "$FAKE_FLOW_LOG"
if [[ "${FAKE_GO_FAIL_ON:-}" == "$kind" ]]; then exit 32; fi
''')
    fake_go.chmod(0o755)
    registry = json.loads((root / 'modules/registry.yaml').read_text())
    tables = sorted(table for module in registry['modules'] if module['status'] == 'implemented'
                    for table in module['owns_tables'])
    env = dict(os.environ, DATABASE_URL='postgresql://fake/opensite',
               FAKE_TABLES_JSON=json.dumps(tables),
               FAKE_PSQL_LOG=str(log), FAKE_FLOW_LOG=str(flow),
               FAKE_FAIL_ON='', FAKE_GO_FAIL_ON='',
               PATH=str(binpath)+os.pathsep+os.environ['PATH'])
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

    migration_calls = 2 * (n_migrations + 1)
    assert flow.read_text().splitlines() == ['INSTALL', 'INSTALL'] + ['SQL'] * migration_calls + ['VERIFY'] + ['SQL'] * len(expected) + ['CATALOG'], 'River before app DDL; verify before SQL tests; catalog after all SQL'

    # Installer and verification failures must not be masked by later SQL gates.
    for phase in ('INSTALL', 'VERIFY'):
        log.write_text('')
        flow.write_text('')
        env['FAKE_GO_FAIL_ON'] = phase
        failed = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
        assert failed.returncode == 32, (failed.stdout, failed.stderr)
        assert 'RUN tests/' not in failed.stdout
        if phase == 'INSTALL':
            assert log.read_text() == ''
    env['FAKE_GO_FAIL_ON'] = ''

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
    env['FAKE_FAIL_ON'] = 'TAX_EXPECTED_REFUSAL'
    failed = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
    assert failed.returncode == 31, (failed.stdout, failed.stderr)
    assert 'RUN tests/db_tax.sql' in failed.stdout
    env['FAKE_FAIL_ON'] = 'LEDGER_EXPECTED_REFUSAL'
    failed = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
    assert failed.returncode == 31, (failed.stdout, failed.stderr)
    assert 'RUN tests/db_ledger.sql' in failed.stdout
    assert 'PASS: module ownership' not in failed.stdout
    env['FAKE_FAIL_ON'] = 'CUSTOMERS_PASSWORD_UPDATED_AT'
    failed = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
    assert failed.returncode == 31, (failed.stdout, failed.stderr)
    assert 'RUN tests/db_customers.sql' in failed.stdout

    for marker, name in [('C07_EXPECTED_REFUSAL', 'db_catalog_api.sql'),
                         ('ORDERS_GATE', 'db_orders.sql'),
                         ('QUEUE_CROSS_TENANT_READ', 'db_queue.sql'),
                         ('RELIABILITY_LIMITS_GATE', 'db_reliability_limits.sql'),
                         ('CART_EXPECTED_REFUSAL', 'db_cart.sql'),
                         ('QUOTES_SCHEMA_REQUIRED', 'db_quotes.sql'),
                         ('OPS_METRICS_GATE', 'db_ops_metrics.sql'),
                         ('IDENTITY_GATE', 'db_identity_auth.sql'),
                         ('LOGIN_GATE', 'db_identity_login.sql'),
                         ('CMS_EXPECTED_REFUSAL', 'db_cms_publish.sql'),
                         ('checkout SQL PASS', 'db_checkout.sql'),
                         ('CUSTOMER_AUTH_GLOBAL_SCHEMA_MISSING', 'db_customer_auth.sql'),
                         ('PAYMENTS_GATE', 'db_payments.sql')]:
        env['FAKE_FAIL_ON'] = marker
        failed = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
        assert failed.returncode == 31, (failed.stdout, failed.stderr)
        assert f'RUN tests/{name}' in failed.stdout
        assert 'PASS: module ownership' not in failed.stdout
# check_pg_upgrade.sh: bad() only sets fail=1, so every bad() call must precede the
# final `[[ $fail -eq 0 ]] || exit 1`, and that check must precede the overall PASS line.
upgrade_lines = (root / 'scripts' / 'check_pg_upgrade.sh').read_text().splitlines()
last_bad = max(i for i, line in enumerate(upgrade_lines) if re.search(r'(^|[|&;{ ])bad ', line))
final_check = max(i for i, line in enumerate(upgrade_lines) if line.strip() == '[[ $fail -eq 0 ]] || exit 1')
final_pass = max(i for i, line in enumerate(upgrade_lines) if line.startswith("echo 'PASS: security"))
assert last_bad < final_check < final_pass, ('upgrade failures after the final exit check', last_bad + 1, final_check + 1, final_pass + 1)
print(f'PASS: check_pg submits all {len(expected)} real-PG test files, repeats {n_migrations} migrations, preserves River ordering and propagates failures (mock go/psql; NO SQL executed)')
