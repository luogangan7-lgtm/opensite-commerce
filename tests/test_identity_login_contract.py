#!/usr/bin/env python3
"""Identity contract gate: anonymous challenge and global session endpoint parity.

Depends on the JSON registry, inline OAS and governance extractor; called by
check_all. Invariant: every implemented identity operation has one OAS owner;
login CSRF remains explicit and self-session operations never demand tenant scope.
Tenant-owned API key management requires an admin session and a finite scope
allowlist; only creation can return the one-time secret.
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
KEY_PATH = '/api/v1/t/{tenant_id}/api-keys'
KEY_OPERATIONS = {
    (KEY_PATH, 'post'): 'createAPIKey',
    (KEY_PATH, 'get'): 'listAPIKeys',
    (KEY_PATH + '/{key_id}', 'delete'): 'revokeAPIKey',
}
SCOPES = {
    'customers:privacy', 'catalog:import', 'catalog:export',
    'apikeys:write', 'catalog:read', 'catalog:write', 'cms:publish',
    'cms:write', 'media:write', 'notifications:write', 'orders:read',
    'quotes:read', 'quotes:write', 'organization.create', 'site.create',
    'fulfillment:write', 'returns:write',
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
    tenant = next(m for m in registry['modules'] if m['name'] == 'tenant')
    assert tenant['go_status'] == 'implemented', 'tenant key management implementation missing'
    assert set(tenant['owns_tables']) == {'tenants', 'tenant_memberships', 'api_keys'}
    assert tenant['dependencies'] == ['identity'], 'tenant depends on the public identity contract'
    assert tenant['scopes'] == ['apikeys:write']
    assert {o['operation_id']: o['status'] for o in tenant['operations']} == {
        operation: 'implemented' for operation in KEY_OPERATIONS.values()}
    for key, operation in KEY_OPERATIONS.items():
        body = blocks[key]
        assert body['operationId'] == operation and body['x-opensite-status'] == 'implemented', key
        assert body['x-opensite-access'] == 'AdminSession', key
        assert body['security'] == [{'adminSession': []}], 'keys cannot manage other keys'
        assert body['x-required-scope'] == 'apikeys:write', key
        refs = {p.get('$ref') for p in body.get('parameters', [])}
        assert '#/components/parameters/TenantId' in refs, key
        assert '#/components/parameters/IdempotencyKey' not in refs, 'secret replay is forbidden'
        if key[1] in {'post', 'delete'}:
            assert {'#/components/parameters/CSRFToken', '#/components/parameters/Origin'} <= refs, key
    revoke = blocks[(KEY_PATH + '/{key_id}', 'delete')]['responses']
    assert '204' in revoke
    assert revoke.get('400') == {'$ref': '#/components/responses/BadRequest'}, 'malformed key ID must be modeled'
    schemas = components['schemas']
    create = schemas['CreateAPIKey']
    assert create['additionalProperties'] is False
    assert create['required'] == ['scopes']
    assert set(create['properties']) == {'scopes', 'expires_at'}
    assert create['properties']['expires_at']['format'] == 'date-time'
    assert create['properties']['scopes']['type'] == 'array'
    assert create['properties']['scopes']['maxItems'] == len(SCOPES), 'every known scope fits in one key'
    assert set(create['properties']['scopes']['items']['enum']) == SCOPES, 'unknown scopes must fail closed'
    # Buyer capabilities are not scopes that a staff administrator can issue.
    staff_blocks = [body for body in blocks.values()
                    if body.get('x-opensite-access') in {'AdminSession', 'AdminOrAPIKey'}]
    assert {body['x-required-scope'] for body in staff_blocks if 'x-required-scope' in body} == SCOPES
    view = schemas['APIKeyView']
    assert view['additionalProperties'] is False
    assert set(view['required']) == {'id', 'key_prefix', 'scopes', 'issued_by', 'created_at'}
    assert set(view['properties']) == {'id', 'key_prefix', 'scopes', 'issued_by', 'created_at', 'expires_at', 'revoked_at', 'last_used_at'}
    created = schemas['APIKeyCreated']
    assert created['additionalProperties'] is False and set(created['required']) == {'key', 'api_key'}
    assert set(created['properties']) == {'key', 'api_key'}
    assert created['properties']['key']['type'] == 'string'
    assert created['properties']['api_key'] == {'$ref': '#/components/schemas/APIKeyView'}
    listed = schemas['APIKeyList']
    assert listed['additionalProperties'] is False and listed['required'] == ['items']
    assert set(listed['properties']) == {'items'}
    assert listed['properties']['items'] == {'type': 'array', 'maxItems': 100, 'items': {'$ref': '#/components/schemas/APIKeyView'}}
    post = blocks[(KEY_PATH, 'post')]
    assert post['requestBody']['content']['application/json']['schema'] == {'$ref': '#/components/schemas/CreateAPIKey'}
    assert post['responses']['201']['content']['application/json']['schema'] == {'$ref': '#/components/schemas/APIKeyCreated'}
    assert blocks[(KEY_PATH, 'get')]['responses']['200']['content']['application/json']['schema'] == {'$ref': '#/components/schemas/APIKeyList'}


registry = json.loads((ROOT / 'modules/registry.yaml').read_text())
source = (ROOT / 'api/openapi.yaml').read_text()
assert_contract(registry, source)
check_operations(registry, ROOT / 'api/openapi.yaml')
# Inject faults into in-memory/temporary copies, preserving the shared checkout.
api = yaml.safe_load(source)
for fault in ['anonymous session', 'login CSRF removed', 'planned status changed', 'actual status changed',
              'bearer key management', 'key CSRF removed', 'unknown scope', 'unknown staff route scope',
              'secret in list', 'secret replay']:
    bad_api = copy.deepcopy(api)
    bad_registry = copy.deepcopy(registry)
    if fault == 'anonymous session':
        bad_api['paths']['/api/v1/auth/session']['get']['security'] = []
    elif fault == 'login CSRF removed':
        bad_api['paths']['/api/v1/auth/login']['post']['parameters'] = []
    elif fault == 'bearer key management':
        bad_api['paths'][KEY_PATH]['get']['security'] = [{'apiKeyAuth': []}]
    elif fault == 'key CSRF removed':
        bad_api['paths'][KEY_PATH + '/{key_id}']['delete']['parameters'] = [{'$ref': '#/components/parameters/TenantId'}]
    elif fault == 'unknown scope':
        bad_api['components']['schemas']['CreateAPIKey']['properties']['scopes']['items']['enum'].append('unknown:scope')
    elif fault == 'unknown staff route scope':
        bad_api['paths']['/api/v1/t/{tenant_id}/quotes/{quote_id}']['get']['x-required-scope'] = 'unknown:scope'
    elif fault == 'secret in list':
        bad_api['components']['schemas']['APIKeyView']['properties']['key'] = {'type': 'string'}
    elif fault == 'secret replay':
        bad_api['paths'][KEY_PATH]['post']['parameters'].append({'$ref': '#/components/parameters/IdempotencyKey'})
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
print('PASS: global sessions, tenant admin-only API keys, exhaustive scope allowlist and ten injected contract faults')
