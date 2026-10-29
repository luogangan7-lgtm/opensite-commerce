#!/usr/bin/env python3
"""CI prerequisite negatives: execute the real prefix block with each input absent.

Depends on migrated isolated PostgreSQL; every rename and patch rolls back.
Called by mdel03-orders CI; absent prerequisites must fail with their own name.
"""
import os
from pathlib import Path
import re
import subprocess

PSQL = ['psql', os.environ['DATABASE_URL'], '-X', '-At', '-v', 'ON_ERROR_STOP=1']
paths = list(Path('migrations').rglob('*_orders_merchant_deletion.sql'))
assert len(paths) == 1, 'Orders migration must resolve uniquely before/after landing'
prefix = re.search(r'DO \$prefix\$.*?END \$prefix\$;', paths[0].read_text(), re.S)[0]
missing = []
for name in ('orders_context', 'orders_create', 'orders_check_creation', 'orders_lock_reservations',
             'orders_transition', 'orders_fulfillment_fence', 'orders_fulfillment_advance',
             'orders_refund_budget', 'orders_finish_return_refund', 'orders_compensate_refund'):
    signatures = subprocess.check_output(PSQL, input=(
        "SELECT oid::regprocedure FROM pg_proc WHERE pronamespace='app'::regnamespace "
        "AND proname='" + name + "';"), text=True, timeout=30).splitlines()
    assert signatures, 'fixture prerequisite missing: ' + name
    renames = '\n'.join('ALTER FUNCTION ' + sig + ' RENAME TO mdel03_missing_' + name + ';'
                        for sig in signatures)
    result = subprocess.run(PSQL, input='BEGIN;\n' + renames + '\n' + prefix + '\nROLLBACK;',
                            text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=90)
    marker = 'Orders prefix functions missing: ' + name
    if result.returncode == 0 or marker not in result.stdout:
        missing.append(name)
        print('MDEL03_MISSING_PREFIX_ACCEPTED ' + name + '\n' + result.stdout, flush=True)
    else:
        print('PASS missing prerequisite rejected: ' + name, flush=True)
assert not missing, 'MDEL03_MISSING_PREFIX_ACCEPTED: ' + ', '.join(missing)
