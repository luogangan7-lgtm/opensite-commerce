#!/usr/bin/env python3
"""Lightweight static contract sanity checks, NOT sqlc generate or SQL parsing."""
from pathlib import Path
import re
import yaml

root = Path(__file__).resolve().parents[1]
config = yaml.safe_load((root/'sqlc.yaml').read_text())
assert config['version'] == '2'
assert len(config['sql']) == 1
sql = config['sql'][0]
assert sql['engine'] == 'postgresql'
assert sql['gen']['go']['sql_package'] == 'pgx/v5'
assert sql['gen']['go']['out'] == 'internal/db'
assert set(sql['schema']) == {str(p.relative_to(root)) for p in (root/'migrations').glob('*.sql') if p.name[:3] not in ('008','009')}
for suffix in ('_security_hardening.sql', '_platform_reliability.sql', '_money_pricing.sql', '_shipping.sql'):
    matches = list((root/'migrations').glob('[0-9][0-9][0-9]'+suffix))
    assert len(matches) == 1, suffix
    assert str(matches[0].relative_to(root)) in sql['schema'], suffix
# New DDL must participate in real codegen, independent of provisional number.
expected_schema = sorted(m.relative_to(root).as_posix() for m in (root/'migrations').glob('[0-9][0-9][0-9]_*.sql')
                         if not m.name.startswith(('008_', '009_')))
assert sorted(sql['schema']) == expected_schema, (sql['schema'], expected_schema)
inventory = list((root/'migrations').glob('*_inventory.sql'))
assert len(inventory) == 1
assert inventory[0].relative_to(root).as_posix() in sql['schema']
for path in sql['schema']:
    assert (root/path).is_file(), path
assert all('008_' not in name and '009_' not in name for name in sql['schema'])
all_schema = '\n'.join((root / name).read_text() for name in sql['schema'])
tables = set(re.findall(r'\bCREATE\s+TABLE\s+app\.([a-z_]+)', all_schema, re.I))
functions = set(re.findall(r'\bCREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+app\.([a-z_]+)', all_schema, re.I))
assert len(tables) >= 25, f'only {len(tables)} tables'
assert {'warehouses','stock_levels','inventory_reservations','inventory_movements'} <= tables
query_names = []
query_text = []
def check_app_references(query, label):
    # FROM can address a set-returning function, not just a table. Validate
    # every app-qualified call against DDL; real sqlc checks SQL/type semantics.
    source = re.sub(r"'(?:[^']|'')*'|--[^\n]*|/\*.*?\*/", '', query, flags=re.S)
    for operation, name, call in re.findall(r'\b(?:(FROM|INTO|JOIN|UPDATE)\s+)?app\.([a-z_]+)\s*(\()?', source, re.I):
        function_call = call and operation.upper() not in ('INTO', 'UPDATE')
        assert name in (functions if function_call else tables), (label, name)

check_app_references('SELECT * FROM app.claim_outbox(NULL,NULL,1,30)', 'known function')
check_app_references("INSERT INTO app.products(tenant_id) VALUES(current_setting('app.tenant_id'))", 'table with columns and GUC')
for invalid in ('SELECT * FROM app.c04b_missing_function()',
                'SELECT * FROM app.c04b_missing_table'):
    try:
        check_app_references(invalid, 'negative fixture')
    except AssertionError:
        pass
    else:
        raise AssertionError('unknown app object accepted')
for qpath in (root/sql['queries']).glob('*.sql'):
    q = qpath.read_text()
    query_text.append(q)
    query_names += re.findall(r'^-- name: (\w+) :(one|many|exec|execrows)',q,re.M)
    check_app_references(q, qpath.name)
assert len(query_names) == len(set(name for name,_ in query_names)), 'duplicate SQL query names'
assert len(query_names) >= 6
ci = (root/'.github/workflows/checks.yml').read_text()
gov = re.search(r'^go (\d+\.\d+\.\d+)$', (root/'go.mod').read_text(), re.M).group(1)
assert f"go-version: '{gov}'" in ci, 'CI go-version must equal go.mod go directive'
assert './scripts/check_codegen.sh' in ci
assert ci.index('name: PostgreSQL 18 forward-only') < ci.index('name: Go pgx-tagged'), 'real generated-query race tests require migrated PostgreSQL'
codegen=(root/'scripts/check_codegen.sh').read_text()
assert 'sqlc generate' in codegen and '-tags pgx' in codegen
print(f'PASS: static sqlc config/query sanity: {len(tables)} schema tables, {len(query_names)} named queries (NOT sqlc generate)')
