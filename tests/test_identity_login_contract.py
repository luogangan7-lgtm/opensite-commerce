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
    'customers:privacy', 'catalog:import', 'catalog:export', 'tenant:delete',
    'apikeys:write', 'catalog:read', 'catalog:write', 'cms:publish',
    'cms:write', 'media:write', 'notifications:write', 'orders:read',
    'quotes:read', 'quotes:write', 'organization.create', 'site.create',
    'fulfillment:write', 'returns:write',
}
NON_STAFF_SCOPES = {
    'quotes:convert': 'Buyer-company conversion capability; never staff/API-key authority.',
}


def assert_contract(registry, source):
    identity = next(m for m in registry['modules'] if m['name'] == 'identity')
    assert identity['go_status'] == 'implemented', 'identity Go implementation missing'
    assert set(identity['owns_tables']) == {'users', 'auth_sessions', 'auth_account_attempts', 'auth_login_attempts', 'known_devices', 'identity_merchant_proofs'}
    assert identity['scopes'] == ['tenant:delete'], 'only dormant tenant-bound step-up has a scope'
    assert {o['operation_id']: o['status'] for o in identity['operations']} == {
        **{operation: 'implemented' for operation in EXPECTED.values()},
        **{operation: 'planned' for operation in ['createAdminSession', 'deleteAdminSession', 'getCurrentActor', 'listAdminSessions', 'revokeAdminSession', 'merchantDeletionStepUp']}}
    api = yaml.safe_load(source)
    blocks = {(path, method): op for path, item in api['paths'].items()
              for method, op in item.items()
              if method in {'get', 'post', 'put', 'delete', 'patch', 'head', 'options', 'trace'}}
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
    assert set(tenant['owns_tables']) == {'tenants', 'tenant_memberships', 'api_keys', 'tenant_lifecycles', 'tenant_lifecycle_receipts', 'tenant_lifecycle_events'}
    assert tenant['dependencies'] == ['identity'], 'tenant depends on the public identity contract'
    assert tenant['scopes'] == ['apikeys:write', 'tenant:delete']
    assert {o['operation_id']: o['status'] for o in tenant['operations']} == {
        **{operation: 'implemented' for operation in KEY_OPERATIONS.values()},
        'requestMerchantDeletion': 'planned', 'getMerchantDeletion': 'planned'}
    deletion = blocks[('/api/v1/t/{tenant_id}/merchant-deletion', 'post')]
    assert deletion['x-opensite-status'] == 'planned'
    assert deletion['x-opensite-access'] == 'AdminSession'
    assert deletion['security'] == [{'adminSession': []}]
    assert deletion['x-required-scope'] == 'tenant:delete'
    assert deletion['responses']['202'].get('content', {}).get('application/json', {}).get('schema') == {'$ref': '#/components/schemas/MerchantDeletionMetadata'}
    receipt = blocks.get(('/api/v1/t/{tenant_id}/merchant-deletion/{request_id}', 'get'))
    assert receipt is not None, 'MDEL02_RECEIPT_GET_CONTRACT'
    assert receipt['operationId'] == 'getMerchantDeletion' and receipt['x-opensite-status'] == 'planned'
    assert receipt['x-opensite-access'] == 'AdminSession' and receipt['security'] == [{'adminSession': []}]
    assert receipt['x-required-scope'] == 'tenant:delete'
    assert receipt['responses']['200']['content']['application/json']['schema'] == {'$ref': '#/components/schemas/MerchantDeletionMetadata'}
    for status, response in [('401', 'Unauthorized'), ('403', 'Forbidden'), ('404', 'NotFound')]:
        assert receipt['responses'][status] == {'$ref': '#/components/responses/' + response}
    step_up = blocks[('/api/v1/t/{tenant_id}/merchant-deletion/step-up', 'post')]
    assert step_up['operationId'] == 'merchantDeletionStepUp' and step_up['x-opensite-status'] == 'planned'
    assert step_up['x-opensite-access'] == 'AdminSession' and step_up['security'] == [{'adminSession': []}]
    assert step_up['x-required-scope'] == 'tenant:delete'
    assert set(components['schemas']['MerchantDeletionStepUpResult']['properties']) == {'proof', 'expires_at'}
    assert deletion['requestBody']['content']['application/json']['schema']['properties']['step_up_proof'] == components['schemas']['MerchantDeletionStepUpResult']['properties']['proof']
    metadata = components['schemas']['MerchantDeletionMetadata']
    fields = {'request_id', 'state', 'phase', 'epoch', 'requested_at', 'execute_after', 'attempt', 'next_retry_at', 'categories'}
    assert metadata['type'] == 'object' and metadata['additionalProperties'] is False
    assert set(metadata['properties']) == fields and set(metadata['required']) == fields, 'MDEL01 metadata is a closed non-PII allowlist'
    assert metadata['properties']['request_id'] == {'type': 'string', 'format': 'uuid'}
    assert metadata['properties']['state']['enum'] == ['active', 'cooling_off', 'executing', 'failed', 'deleted']
    assert metadata['properties']['execute_after'] == {'type': 'string', 'format': 'date-time', 'maxLength': 40}
    counts = metadata['properties']['categories']
    assert counts['type'] == 'object' and counts['additionalProperties'] is False and counts['maxProperties'] == 8
    assert set(counts['properties']) == {'paid_unfulfilled', 'fulfillment_return', 'payment_refund', 'checkout_capture', 'refund_reconciliation', 'pending_payout', 'unsettled_provider_balance', 'legal_hold'}
    for count in counts['properties'].values():
        assert count == {'type': 'integer', 'format': 'int64', 'minimum': 0, 'maximum': 999999999999999999}, 'MDEL01 bounded category counts'
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
    # Access classes must never hide scopes from the canonical inventory.
    scoped_blocks = [body for body in blocks.values() if 'x-required-scope' in body]
    declared_scopes = {body['x-required-scope'] for body in scoped_blocks}
    assert not SCOPES & NON_STAFF_SCOPES.keys(), 'non-staff exceptions cannot exempt staff scopes'
    assert declared_scopes == SCOPES | NON_STAFF_SCOPES.keys(), (
        f'route scope parity: unknown={sorted(declared_scopes - SCOPES - NON_STAFF_SCOPES.keys())}, '
        f'stale={sorted((SCOPES | NON_STAFF_SCOPES.keys()) - declared_scopes)}')
    for body in scoped_blocks:
        scope = body['x-required-scope']
        if scope in NON_STAFF_SCOPES:
            assert body.get('x-opensite-access') == 'BuyerCompany', 'buyer scope escaped to another access class'
        owner = next((m for m in registry['modules']
                      if any(op['operation_id'] == body['operationId'] for op in m['operations'])), None)
        assert owner is not None, f"registry owner missing for scoped operation {body['operationId']}"
        assert scope in owner['scopes'], f"module {owner['name']} missing scope {scope} for {body['operationId']}"
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
identity_migration = next((ROOT / 'migrations/pending').glob('mdel02-identity_*.sql')).read_text()
assert "p.proname='tenant_lifecycle_transition'" not in identity_migration, 'MDEL02_TENANT_GUARD_OWNERSHIP: Identity rewrites Tenant private transition'
assert_contract(registry, source)
check_operations(registry, ROOT / 'api/openapi.yaml')
# Inject faults into in-memory/temporary copies, preserving the shared checkout.
api = yaml.safe_load(source)
for fault in ['anonymous session', 'login CSRF removed', 'planned status changed', 'actual status changed',
              'bearer key management', 'key CSRF removed', 'unknown scope', 'unknown staff route scope',
              'secret in list', 'secret replay', 'stale buyer exception', 'buyer scope on staff route',
              'module scope missing', 'module operation missing',
              'merchant metadata missing', 'merchant contact field', 'merchant unbounded counts']:
    bad_api = copy.deepcopy(api)
    bad_registry = copy.deepcopy(registry)
    if fault == 'merchant metadata missing':
        del bad_api['paths']['/api/v1/t/{tenant_id}/merchant-deletion']['post']['responses']['202']['content']
    elif fault == 'merchant contact field':
        bad_api['components']['schemas']['MerchantDeletionMetadata']['properties']['contact'] = {'type': 'string'}
    elif fault == 'merchant unbounded counts':
        del bad_api['components']['schemas']['MerchantDeletionMetadata']['properties']['categories']['properties']['pending_payout']['maximum']
    elif fault == 'anonymous session':
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
    elif fault == 'stale buyer exception':
        del bad_api['paths']['/api/v1/t/{tenant_id}/quotes/{quote_id}/orders']['post']['x-required-scope']
    elif fault == 'buyer scope on staff route':
        bad_api['paths']['/api/v1/t/{tenant_id}/quotes/{quote_id}']['get']['x-required-scope'] = 'quotes:convert'
    elif fault == 'module scope missing':
        next(m for m in bad_registry['modules'] if m['name'] == 'quotes')['scopes'].remove('quotes:convert')
    elif fault == 'module operation missing':
        quotes = next(m for m in bad_registry['modules'] if m['name'] == 'quotes')
        quotes['operations'] = [op for op in quotes['operations'] if op['operation_id'] != 'getQuote']
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
# Probe every access class, including classes outside staff authentication.
for method, access in [
        ('get', access) for access in ['AdminSession', 'AdminOrAPIKey', 'APIKey', 'InternalService',
                                     'AuthenticatedSelf', 'BuyerCompany', 'CustomerSession', 'Storefront', 'Public']
        ] + [(method, 'APIKey') for method in ['head', 'options', 'trace']]:
    bad_api = copy.deepcopy(api)
    bad_api['paths']['/scope-parity-probe'] = {method: {
        'operationId': 'scopeParityProbe', 'x-opensite-access': access,
        'x-required-scope': 'unknown:scope',
    }}
    try:
        assert_contract(registry, yaml.safe_dump(bad_api))
    except AssertionError:
        pass
    else:
        raise AssertionError(f'unknown scope accepted for {method} / {access}')
with tempfile.TemporaryDirectory() as tmp:
    bad = Path(tmp) / 'openapi.yaml'
    bad.write_text(source.replace('operationId: createStaffLoginSession', 'operationId: orphanSession'))
    try:
        check_operations(registry, bad)
    except GovernanceError:
        pass
    else:
        raise AssertionError('missing identity operation accepted')
print('PASS: global sessions, tenant admin-only API keys, all-route scope parity, exact buyer exceptions, module ownership and injected contract faults')
