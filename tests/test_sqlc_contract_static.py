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
# New DDL must participate in real codegen, independent of provisional number.
assert len(sql['schema']) == 8
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
query_names = []
query_text = []
for qpath in (root/sql['queries']).glob('*.sql'):
    q = qpath.read_text()
    query_text.append(q)
    query_names += re.findall(r'^-- name: (\w+) :(one|many|exec|execrows)',q,re.M)
    for table in re.findall(r'\b(?:FROM|INTO|JOIN|UPDATE)\s+app\.([a-z_]+)',q,re.I):
        assert table in tables, (qpath.name,table)
assert len(query_names) == len(set(name for name,_ in query_names)), 'duplicate SQL query names'
assert len(query_names) >= 6
ci = (root/'.github/workflows/checks.yml').read_text()
assert "go-version: '1.26.x'" in ci, 'pgx requires Go >=1.25'
assert './scripts/check_codegen.sh' in ci
codegen=(root/'scripts/check_codegen.sh').read_text()
assert 'sqlc generate' in codegen and '-tags pgx' in codegen
print(f'PASS: static sqlc config/query sanity: {len(tables)} schema tables, {len(query_names)} named queries (NOT sqlc generate)')
