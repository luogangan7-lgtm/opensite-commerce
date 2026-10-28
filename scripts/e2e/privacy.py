"""CI privacy wiring witness; called by run.py and the disposable ops drill.

Seeds identities/authorization only. Production HTTP/worker own export state;
private capability values never enter logs or receipts. Uses stdlib transport.
"""
import base64
import hashlib
import json
import secrets
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


def environment(password):
    # Both harnesses generate hex/base64url passwords; never accept DSN options.
    if not password or any(c not in 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_' for c in password):
        raise RuntimeError('PRIVACY_WIRING: invalid fixture credential')
    dsn = 'host=postgres port=5432 dbname=opensite user=opensite_runtime_login password=' + password + ' sslmode=disable'
    return {'OPENSITE_PRIVACY_DATABASE_URL': dsn, 'PRIVACY_WORKER_DATABASE_URL': dsn,
            'PRIVACY_TOKEN_KEY': secrets.token_hex(32), 'PRIVACY_EXPORT_WORKER_ENABLED': 'true'}


def prepare(url, psql, tenant, credentials=None, enabled=True):
    tenant = str(uuid.UUID(tenant))
    staff, customer, key = (str(uuid.uuid4()) for _ in range(3))
    secret = secrets.token_bytes(32)
    bearer = key + '.' + base64.urlsafe_b64encode(secret).decode().rstrip('=')
    if credentials is not None:
        credentials['PRIVACY_WITNESS_BEARER'] = bearer
    psql(f"""BEGIN;
      INSERT INTO app.users(id,email,password_hash)
        VALUES ('{staff}','privacy-{staff}@example.test','fixture-not-a-password');
      INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES ('{tenant}','{staff}','admin');
      INSERT INTO app.api_keys(tenant_id,id,issued_by,key_prefix,key_hash,scopes)
        VALUES ('{tenant}','{key}','{staff}','privacy-ci',decode('{hashlib.sha256(secret).hexdigest()}','hex'),ARRAY['customers:privacy']);
      INSERT INTO app.customers(tenant_id,id,email,display_name)
        VALUES ('{tenant}','{customer}','privacy-{customer}@example.test','Privacy wiring witness');
      COMMIT;""")
    witness = dict(url=url, tenant=tenant, customer=customer, bearer=bearer,
                   base=f'/api/v1/t/{tenant}/customers/{customer}/data-exports')
    created = request(witness, witness['base'], 202 if enabled else 503, method='POST')
    if not enabled:
        return witness
    witness['request'] = str(uuid.UUID(created['request_id']))
    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        status = request(witness, witness['base'] + '/' + witness['request'], 200)
        if status.get('status') == 'ready':
            link = status.get('download_url', '')
            parsed = urllib.parse.urlsplit(link)
            if parsed.scheme or parsed.netloc or parsed.path != witness['base'] + '/' + witness['request'] + '/download':
                raise RuntimeError('PRIVACY_WIRING: invalid download capability')
            witness['download'] = link
            return witness
        time.sleep(.2)
    raise RuntimeError('PRIVACY_WIRING: worker did not complete export')


def request(witness, path, expected, method='GET'):
    headers = {'Authorization': 'Bearer ' + witness['bearer'],
               'Idempotency-Key': str(uuid.uuid4())}
    req = urllib.request.Request(witness['url'] + path, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=10) as response:
            raw = response.read(10 * 1024 * 1024 + 1)
            if response.status != expected or len(raw) > 10 * 1024 * 1024 or response.headers.get('Cache-Control') != 'no-store':
                raise RuntimeError('PRIVACY_WIRING: unexpected response')
            return json.loads(raw)
    except urllib.error.HTTPError as error:
        if error.code == expected:
            return {}
        raise RuntimeError(f'PRIVACY_WIRING: HTTP {error.code}, expected {expected}') from None
    except (OSError, ValueError):
        raise RuntimeError('PRIVACY_WIRING: transport or export content invalid') from None


def download(witness, url=None):
    current = {**witness, 'url': url or witness['url']}
    body = request(current, current['download'], 200, method='POST')
    if not isinstance(body, dict) or current['customer'] not in json.dumps(body):
        raise RuntimeError('PRIVACY_WIRING: downloaded export lost customer identity')
    return {'status': 'PASS', 'api': 202, 'worker': 'ready', 'shared_export_download': 200}
