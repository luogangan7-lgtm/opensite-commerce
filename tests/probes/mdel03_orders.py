#!/usr/bin/env python3
"""MDEL-03 assertion-specific fault probes; isolated GitHub CI database only.

Mutate real installed owner functions, require each exact failing assertion,
restore the original source in finally, then demand the clean baseline again.
"""
import os
import subprocess

PSQL = ['psql', os.environ['DATABASE_URL'], '-X', '-At', '-v', 'ON_ERROR_STOP=1']


def command(sql):
    return subprocess.check_output(PSQL, input=sql, text=True,
                                   stderr=subprocess.STDOUT, timeout=90)


def test(marker=None):
    result = subprocess.run(PSQL + ['-f', 'tests/db_orders_merchant_deletion.sql'],
                            text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, timeout=90)
    print(result.stdout, flush=True)
    if marker is None:
        assert result.returncode == 0, 'baseline failed'
    else:
        assert result.returncode != 0 and marker in result.stdout, 'mutation missed assertion: ' + marker
        print('CAUSAL_RED ' + marker, flush=True)


test()
scrub = 'orders_erase_merchant(uuid,uuid,bigint)'
# Isolated CI only: defeat FORCE RLS so it cannot conceal a missing WHERE.
assert command("SELECT rolbypassrls FROM pg_roles WHERE rolname='app_orders_privacy_executor';").strip() == 'f'
source = command("SELECT pg_get_functiondef('app." + scrub + "'::regprocedure);")
try:
    command('ALTER ROLE app_orders_privacy_executor BYPASSRLS;')
    test()
    for old, marker in [
        ("UPDATE app.order_adjustments SET reason='[erased]' WHERE tenant_id=p_tenant;",
         'MDEL03_ADJUSTMENTS_TENANT_PREDICATE'),
        ('UPDATE app.order_lines SET sku=\'[erased]\',tax_snapshot=\'{"erased":true}\'::jsonb WHERE tenant_id=p_tenant;',
         'MDEL03_LINES_TENANT_PREDICATE'),
        ("shipping_snapshot=jsonb_build_object('erased',true,'amount_minor',shipping_minor,'currency',currency) WHERE tenant_id=p_tenant;",
         'MDEL03_HEADERS_TENANT_PREDICATE'),
    ]:
        assert source.count(old) == 1, 'predicate mutation anchor drift'
        try:
            command(source.replace(old, old.replace(' WHERE tenant_id=p_tenant', '')))
            test(marker)
        finally:
            command(source)
finally:
    command('ALTER ROLE app_orders_privacy_executor NOBYPASSRLS;')
test()
for function, old, new, marker in [
    ('orders_merchant_mutation_fence(boolean)',
     'PERFORM app.tenant_lifecycle_write_fence(t);',
     'PERFORM app.tenant_lifecycle_read_fence(t);', 'MDEL03_COOLING_NEW_WRITE'),
    ('orders_erase_merchant(uuid,uuid,bigint)',
     'UPDATE app.order_lines SET sku=\'[erased]\',tax_snapshot=\'{"erased":true}\'::jsonb WHERE tenant_id=p_tenant;',
     'PERFORM 1;', 'MDEL03_DERIVED_SCRUB'),
    ('orders_fulfillment_advance(uuid,text)',
     "p_event=r.status||':'||r.id::text||':'||encode(sha256(convert_to(r.refund_key,'UTF8')),'hex')",
     'false', 'MDEL03_COOLING_FAILED_REFUND_DRAIN'),
]:
    source = command("SELECT pg_get_functiondef('app." + function + "'::regprocedure);")
    assert source.count(old) == 1, 'mutation anchor drift'
    try:
        command(source.replace(old, new))
        test(marker)
    finally:
        command(source)
test()
print('PASS: MDEL03 six exact CAUSAL_RED assertions and restored baseline', flush=True)
