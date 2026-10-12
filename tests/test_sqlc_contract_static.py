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
for suffix in ('_security_hardening.sql', '_platform_reliability.sql', '_money_pricing.sql', '_shipping.sql', '_tax.sql'):
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
assert len(tables) >= 25, f'only {len(tables)} tables'
assert {'warehouses','stock_levels','inventory_reservations','inventory_movements'} <= tables
functions = set(re.findall(r'\bCREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+app\.([a-z_]+)', all_schema, re.I))


def check_sources(query, origin):
    # FROM/JOIN may invoke a declared set-returning function. INTO's parentheses
    # are column lists, so writes must still name a real table. This is only a
    # name sanity check; pinned sqlc and real PG independently parse/typecheck.
    sources = re.findall(r'\b(FROM|INTO|JOIN|UPDATE)\s+app\.([a-z_]+)\b(\s*\()?', query, re.I)
    for clause, name, call in sources:
        expected = functions if call and clause.upper() in ('FROM', 'JOIN') else tables
        assert name in expected, (origin, name)


check_sources('SELECT * FROM app.inventory_reserve($1, $2);', 'function fixture')
check_sources('INSERT INTO app.orders(tenant_id,id) VALUES($1,$2);', 'column-list fixture')
for invalid in ('SELECT * FROM app.missing_function($1);',
                'INSERT INTO app.missing_table(tenant_id) VALUES($1);'):
    try:
        check_sources(invalid, 'negative fixture')
    except AssertionError:
        pass
    else:
        raise AssertionError('static source check accepted an undeclared name')
query_names = []
query_text = []
for qpath in (root/sql['queries']).glob('*.sql'):
    q = qpath.read_text()
    query_text.append(q)
    query_names += re.findall(r'^-- name: (\w+) :(one|many|exec|execrows)',q,re.M)
    check_sources(q, qpath.name)
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

# Pin the assigned-country snapshot, so a coordinated SQL/Go typo cannot pass
# only by preserving list length. Integration must consciously update this pin.
import hashlib
tax_file, = (root/'migrations').glob('[0-9][0-9][0-9]_tax.sql')
country_check = re.search(r'tax_country_assigned CHECK\(country_code IN \((.*?)\)\)', tax_file.read_text()).group(1)
countries = re.findall(r"'([A-Z]{2})'", country_check)
assert countries == sorted(set(countries)) and len(countries) == 249
assert hashlib.sha256(' '.join(countries).encode()).hexdigest() == 'e1c950d24ceb933ac49eaa44de8d1a7ffb6bf40bfe519a5a99d791cb50332197', 'tax assigned-country snapshot changed'
print('PASS: Tax assigned-country snapshot pinned independently of runtime selection')
