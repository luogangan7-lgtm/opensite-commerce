#!/usr/bin/env python3
"""Prevent SQL/Go country allowlist drift; this does not execute PostgreSQL."""
from pathlib import Path
import hashlib
import json
import re

root = Path(__file__).resolve().parents[1]
# Discover by logical name, so integration may assign a new migration number.
migrations = list((root/'migrations').glob('*_shipping.sql'))
assert len(migrations) == 1, 'one shipping migration required'
sql = migrations[0].read_text()
go = (root/'internal/shipping/shipping.go').read_text()
sql_match = re.search(r'shipping_destination_country CHECK\(country_code::text = ANY\(ARRAY\[(.*?)\]::text\[\]\)\)', sql, re.S)
assert sql_match, 'country CHECK missing'
sql_codes = re.findall(r"'([A-Z]{2})'", sql_match[1])
go_match = re.search(r'var countryCodes = strings.Fields\((.*?)\)', go, re.S)
assert go_match, 'Go country registry missing'
go_codes = ''.join(json.loads(s) for s in re.findall(r'"[^"\n]*"', go_match[1])).split()
assert len(sql_codes) == 249 and len(set(sql_codes)) == 249, 'SQL assigned country registry drift'
assert sql_codes == sorted(sql_codes) == go_codes, 'SQL/Go assigned country registry mismatch'
# Pin the reviewed 2026-10-12 registry independently of both production copies.
# A deliberate country-registry update must review and update this receipt too.
assert hashlib.sha256(' '.join(sql_codes).encode()).hexdigest() == 'e1c950d24ceb933ac49eaa44de8d1a7ffb6bf40bfe519a5a99d791cb50332197', 'reviewed country registry changed'
assert {'US','CA','GB','CN','TW','FR','ZA'} <= set(sql_codes)
assert not {'ZZ','UK','EU','XK'} & set(sql_codes)
print('PASS: SQL/Go 249 assigned country codes identical (static, NOT PG execution)')
