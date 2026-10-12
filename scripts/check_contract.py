#!/usr/bin/env python3
"""Offline OpenAPI structural/ref/schema checks, not full OAS spec certification."""
from pathlib import Path
import yaml
from jsonschema import Draft202012Validator, FormatChecker
from jsonschema.exceptions import ValidationError

root=Path(__file__).resolve().parents[1]
api=yaml.safe_load((root/'api/openapi.yaml').read_text())
assert api['openapi'].startswith('3.1.')
assert api.get('paths') and api.get('components')
refs=[]
def traverse(o):
    if isinstance(o,dict):
        if '$ref' in o: refs.append(o['$ref'])
        for v in o.values(): traverse(v)
    elif isinstance(o,list):
        for v in o: traverse(v)
traverse(api)

def resolve(pointer):
    assert pointer.startswith('#/'), f'external/invalid reference: {pointer}'
    item=api
    for seg in pointer[2:].split('/'):
        seg=seg.replace('~1','/').replace('~0','~')
        assert seg in item, f'broken $ref {pointer}'
        item=item[seg]
    return item
for ref in refs: resolve(ref)
seen=set(); operations=0
for path, methods in api['paths'].items():
    assert path.startswith('/'),path
    for method, op in methods.items():
        if method not in ('get','post','patch','put','delete'): continue
        ident=op['operationId']; assert ident not in seen,ident
        seen.add(ident); operations+=1
        assert 'responses' in op, ident
        if method in ('post','put','patch') and method!='put' and 'requestBody' not in op and 'drafts/' not in path and '/complete' not in path:
            print('CHECK optional requestBody:',ident)
# 3.1 Schema Objects reference the 2020-12 dialect.
schemas=api['components']['schemas']
for key, schema in schemas.items():
    Draft202012Validator.check_schema(schema)
validator=Draft202012Validator({'$ref':'#/components/schemas/PageDocument','components':{'schemas':schemas}}, format_checker=FormatChecker())
valid=yaml.safe_load((root/'fixtures/home-valid.json').read_text())
invalid=yaml.safe_load((root/'fixtures/home-invalid.json').read_text())
validator.validate(valid)
try:
    validator.validate(invalid)
except ValidationError:
    pass
else:
    raise AssertionError('malicious component not rejected by schema')
print(f'PASS: YAML parsed; {operations} API operations; {len(refs)} references resolved; {len(schemas)} JSON Schemas; valid+invalid page fixtures checked.')
print('NOT CHECKED: full OpenAPI conformance validator, implemented endpoints, PG migrations.')
