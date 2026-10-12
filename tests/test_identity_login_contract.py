#!/usr/bin/env python3
"""Identity contract gate: anonymous challenge and global session endpoint parity.

Depends on the JSON registry, inline OAS and governance extractor; called by
check_all. Invariant: every implemented identity operation has one OAS owner;
login CSRF remains explicit and self-session operations never demand tenant scope.
This static contract gate does not claim HTTP or PostgreSQL runtime acceptance.
"""
from pathlib import Path
import json
import re
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'scripts'))
from check_modules import GovernanceError, check_operations

EXPECTED = {
    ('/api/v1/auth/login', 'get'): 'getAdminLoginChallenge',
    ('/api/v1/auth/login', 'post'): 'createAdminSession',
    ('/api/v1/auth/logout', 'post'): 'revokeAdminSession',
    ('/api/v1/auth/session', 'get'): 'getAdminSession',
}


def assert_contract(registry, source):
    identity = next(m for m in registry['modules'] if m['name'] == 'identity')
    assert identity['go_status'] == 'implemented', 'identity Go implementation missing'
    assert set(identity['owns_tables']) == {'users', 'auth_sessions', 'auth_account_attempts', 'auth_login_attempts', 'known_devices'}
    assert identity['scopes'] == [], 'global session routes must carry no tenant scopes'
    assert {o['operation_id']: o['status'] for o in identity['operations']} == {
        operation: 'implemented' for operation in EXPECTED.values()}
    blocks = {}
    path = method = None
    for line in source.splitlines():
        if re.match(r'^  /', line):
            path = line.strip().removesuffix(':')
            method = None
        elif re.match(r'^    (get|post|put|delete|patch):$', line):
            method = line.strip().removesuffix(':')
            blocks[(path, method)] = []
        elif path and method:
            blocks[(path, method)].append(line)
    for key, operation in EXPECTED.items():
        body = '\n'.join(blocks[key])
        assert f'operationId: {operation}' in body, key
        assert 'x-required-scope:' not in body and 'tenant_id' not in body, key
        if key[0] == '/api/v1/auth/login':
            assert 'security: []' in body, 'login must be anonymous'
        else:
            assert 'AdminSession: []' in body, 'self endpoints require only admin session'
            assert 'ApiKeyBearer:' not in body, 'bearer key cannot admit admin session endpoints'
        if key[1] == 'post':
            assert "#/components/parameters/CSRFToken" in body and "#/components/parameters/Origin" in body, 'POST requires signed CSRF plus exact Origin'
    components = source.split('\ncomponents:\n', 1)[1]
    assert re.search(r'^  securitySchemes:\n    AdminSession:', components, re.M)
    for parameter in ['LoginChallengeCookie', 'CSRFToken', 'Origin']:
        assert re.search(r'^    '+parameter+r':\n      name:', components, re.M), parameter
    assert 'name: __Host-osc_admin_session' in source
    assert 'name: __Host-osc_login_csrf' in source
    assert "#/components/parameters/LoginChallengeCookie" in '\n'.join(blocks[('/api/v1/auth/login', 'post')])
    assert 'required: [email, password]' in source
    assert 'required: [user_id, session_id, expires_at, csrf_token]' in source


registry = json.loads((ROOT / 'modules/registry.yaml').read_text())
source = (ROOT / 'api/openapi.yaml').read_text()
assert_contract(registry, source)
check_operations(registry, ROOT / 'api/openapi.yaml')
# Inject faults into in-memory/temporary copies, preserving the shared checkout.
for fault, mutation in [('anonymous session', lambda s: s.replace('security: [{ AdminSession: [] }]', 'security: []')),
                        ('login CSRF removed', lambda s: s.replace("- $ref: '#/components/parameters/CSRFToken'", '- name: decoy'))]:
    try:
        assert_contract(registry, mutation(source))
    except AssertionError:
        pass
    else:
        raise AssertionError(f'fault accepted: {fault}')
with tempfile.TemporaryDirectory() as tmp:
    bad = Path(tmp) / 'openapi.yaml'
    bad.write_text(source.replace('operationId: createAdminSession', 'operationId: orphanSession'))
    try:
        check_operations(registry, bad)
    except GovernanceError:
        pass
    else:
        raise AssertionError('missing identity operation accepted')
print('PASS: four identity OAS/registry operations, global-session security and three injected contract faults')
