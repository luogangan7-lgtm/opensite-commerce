#!/usr/bin/env python3
"""Identity contract gate: anonymous challenge and global session endpoint parity.

Depends on the JSON registry, inline OAS and governance extractor; called by
check_all. Invariant: every implemented identity operation has one OAS owner;
login CSRF remains explicit and self-session operations never demand tenant scope.
This static contract gate does not claim HTTP or PostgreSQL runtime acceptance.
"""
from pathlib import Path
import json
import copy
import yaml
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'scripts'))
from check_modules import GovernanceError, check_operations

EXPECTED = {
    ('/api/v1/auth/login', 'get'): 'getAdminLoginChallenge',
    ('/api/v1/auth/login', 'post'): 'createStaffLoginSession',
    ('/api/v1/auth/logout', 'post'): 'logoutCurrentStaffSession',
    ('/api/v1/auth/session', 'get'): 'getAdminSession',
}


def assert_contract(registry, source):
    identity = next(m for m in registry['modules'] if m['name'] == 'identity')
    assert identity['go_status'] == 'implemented', 'identity Go implementation missing'
    assert set(identity['owns_tables']) == {'users', 'auth_sessions', 'auth_account_attempts', 'auth_login_attempts', 'known_devices'}
    assert identity['scopes'] == [], 'global session routes must carry no tenant scopes'
    assert {o['operation_id']: o['status'] for o in identity['operations']} == {
        **{operation: 'implemented' for operation in EXPECTED.values()},
        **{operation: 'planned' for operation in ['createAdminSession', 'deleteAdminSession', 'getCurrentActor', 'listAdminSessions', 'revokeAdminSession']}}
    api = yaml.safe_load(source)
    blocks = {(path, method): op for path, item in api['paths'].items()
              for method, op in item.items() if method in {'get', 'post', 'put', 'delete', 'patch'}}
    for key, operation in EXPECTED.items():
        body = blocks[key]
        assert body['operationId'] == operation, key
        assert body['x-opensite-status'] == 'implemented', key
        assert 'x-required-scope' not in body and '{tenant_id}' not in key[0], key
        if key[0] == '/api/v1/auth/login':
            assert body['security'] == [], 'login must be anonymous'
        else:
            assert body['security'] == [{'adminSession': []}], 'self endpoints require only admin session'
        if key[1] == 'post':
            refs = {p.get('$ref') for p in body.get('parameters', [])}
            assert {'#/components/parameters/CSRFToken', '#/components/parameters/Origin'} <= refs, 'POST requires signed CSRF plus exact Origin'
    components = api['components']
    assert components['securitySchemes']['adminSession']['name'] == '__Host-osc_admin_session'
    for parameter in ['LoginChallengeCookie', 'CSRFToken', 'Origin']:
        assert components['parameters'][parameter]['required'] is True, parameter
    assert components['parameters']['LoginChallengeCookie']['name'] == '__Host-osc_login_csrf'
    assert {'$ref': '#/components/parameters/LoginChallengeCookie'} in blocks[('/api/v1/auth/login', 'post')]['parameters']
    assert components['schemas']['AdminLogin']['required'] == ['email', 'password']
    assert components['schemas']['AdminSessionView']['required'] == ['user_id', 'session_id', 'expires_at', 'csrf_token']


registry = json.loads((ROOT / 'modules/registry.yaml').read_text())
source = (ROOT / 'api/openapi.yaml').read_text()
assert_contract(registry, source)
check_operations(registry, ROOT / 'api/openapi.yaml')
# Inject faults into in-memory/temporary copies, preserving the shared checkout.
api = yaml.safe_load(source)
for fault in ['anonymous session', 'login CSRF removed', 'planned status changed', 'actual status changed']:
    bad_api = copy.deepcopy(api)
    bad_registry = copy.deepcopy(registry)
    if fault == 'anonymous session':
        bad_api['paths']['/api/v1/auth/session']['get']['security'] = []
    elif fault == 'login CSRF removed':
        bad_api['paths']['/api/v1/auth/login']['post']['parameters'] = []
    else:
        identity = next(m for m in bad_registry['modules'] if m['name'] == 'identity')
        name = 'createAdminSession' if fault == 'planned status changed' else 'createStaffLoginSession'
        next(op for op in identity['operations'] if op['operation_id'] == name)['status'] = 'implemented' if fault == 'planned status changed' else 'planned'
    try:
        assert_contract(bad_registry, yaml.safe_dump(bad_api))
    except AssertionError:
        pass
    else:
        raise AssertionError(f'fault accepted: {fault}')
with tempfile.TemporaryDirectory() as tmp:
    bad = Path(tmp) / 'openapi.yaml'
    bad.write_text(source.replace('operationId: createStaffLoginSession', 'operationId: orphanSession'))
    try:
        check_operations(registry, bad)
    except GovernanceError:
        pass
    else:
        raise AssertionError('missing identity operation accepted')
print('PASS: four implemented staff + five planned identity operations, global-session security and five injected contract faults')
