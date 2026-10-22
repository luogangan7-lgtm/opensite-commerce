"""CI purchase fixtures and independent persistence witnesses.

Depends on the production CMS HTTP writer and owned disposable database only;
called by run.py after the idempotent catalog seed. No product implementation.
"""
import base64
import hashlib
import json
import secrets
import urllib.error
import urllib.request
import uuid

TENANT = 'c0900001-0000-4000-8000-000000000001'
SITE = 'c0900003-0000-4000-8000-000000000001'
SKU = 'c2400008-0000-4000-8000-000000000001'


def publish(psql, credentials):
    """Use authenticated CMS HTTP: builder seals membership in its creating TX."""
    key_id, secret = str(uuid.uuid4()), secrets.token_bytes(32)
    bearer = key_id + '.' + base64.urlsafe_b64encode(secret).decode().rstrip('=')
    credentials['C09D_CMS_BEARER'] = bearer
    digest = hashlib.sha256(secret).hexdigest()
    psql(f"""BEGIN;
    INSERT INTO app.tenant_memberships(tenant_id,user_id,role)
      SELECT '{TENANT}',user_id,'owner' FROM app.tenant_memberships WHERE role='owner';
    INSERT INTO app.api_keys(tenant_id,id,issued_by,key_prefix,key_hash,scopes)
      SELECT '{TENANT}','{key_id}',user_id,'c09d-ci',decode('{digest}','hex'),ARRAY['cms:write','cms:publish']
      FROM app.tenant_memberships WHERE tenant_id='{TENANT}' AND role='owner';
    COMMIT;""")

    def post(path, body, etag=None):
        headers = {'Content-Type': 'application/json', 'Authorization': 'Bearer ' + bearer,
                   'Idempotency-Key': 'c09d-' + str(uuid.uuid4())}
        if etag:
            headers['If-Match'] = etag
        request = urllib.request.Request('http://127.0.0.1:8080' + path,
                                        data=json.dumps(body).encode(), headers=headers, method='POST')
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                if response.status != 201:
                    raise RuntimeError('C09d CMS expected creation receipt')
                return json.load(response), response.headers.get('ETag')
        except urllib.error.HTTPError as exc:
            # Problem codes aid diagnosis; neither credential nor request is logged.
            problem = json.loads(exc.read(4096))
            raise RuntimeError(f"C09d CMS {exc.code}: {problem.get('code')}") from None

    try:
        path = f'/api/v1/t/{TENANT}/sites/{SITE}/drafts'
        draft, etag = post(path, {'locale': 'en', 'title': 'OpenSite sample shop',
                                 'description': 'A published example of pages and products.'})
        release, _ = post(path + '/' + str(uuid.UUID(draft['draft_id'])) + '/publish',
                          {'expected_release_id': draft['base_release_id'],
                           'expected_publication_revision': draft['publication_revision'],
                           'renderer_build_id': 'c09d-real-purchase'}, etag)
        release_id = str(uuid.UUID(release['release_id']))
        witness = json.loads(psql(f"""SELECT jsonb_build_object('release_id',r.id,'sealed',r.sealed,
          'members',(SELECT count(*) FROM app.site_release_skus m WHERE (m.tenant_id,m.site_id,m.release_id)=(r.tenant_id,r.site_id,r.id)),
          'serving',(SELECT count(*) FROM app.storefront_serving s WHERE (s.tenant_id,s.site_id,s.release_id)=(r.tenant_id,r.site_id,r.id)))
          FROM app.site_releases r WHERE r.tenant_id='{TENANT}' AND r.site_id='{SITE}' AND r.id='{release_id}';"""))
        if witness != {'release_id': release_id, 'sealed': True, 'members': 2, 'serving': 1}:
            raise RuntimeError('C09d real publication membership/serving witness differs')
        return witness
    finally:
        psql(f"""BEGIN;
        UPDATE app.api_keys SET revoked_at=clock_timestamp() WHERE tenant_id='{TENANT}' AND id='{key_id}';
        DELETE FROM app.tenant_memberships WHERE tenant_id='{TENANT}' AND user_id=(
          SELECT issued_by FROM app.api_keys WHERE tenant_id='{TENANT}' AND id='{key_id}');
        COMMIT;""")


WITNESS_SQL = f"""
WITH c AS (SELECT * FROM app.checkout_sessions WHERE tenant_id='{TENANT}'),
r AS (SELECT r.*,c.phase,c.order_id FROM c JOIN app.order_lines l ON (l.tenant_id,l.order_id)=(c.tenant_id,c.order_id)
 JOIN app.inventory_reservations r ON (r.tenant_id,r.id)=(l.tenant_id,l.reservation_id)),
e AS (SELECT e.*,a.code FROM app.ledger_entries e JOIN app.ledger_accounts a ON (a.tenant_id,a.id)=(e.tenant_id,e.account_id) WHERE e.tenant_id='{TENANT}')
SELECT jsonb_build_object(
 'sessions',(SELECT count(*) FROM c),
 'paid',(SELECT count(*) FROM c JOIN app.orders o ON (o.tenant_id,o.id)=(c.tenant_id,c.order_id) WHERE c.phase='paid' AND o.state='paid' AND o.sealed AND o.total_minor=3140 AND c.amount_minor=3140 AND c.currency='USD'),
 'receipt_only',(SELECT count(*) FROM c JOIN app.orders o ON (o.tenant_id,o.id)=(c.tenant_id,c.order_id) WHERE c.phase='ready' AND o.state='pending_payment'),
 'committed_owned',(SELECT count(*) FROM r WHERE phase='paid' AND state='committed' AND owner_type='order' AND owner_id=order_id AND holder_hash IS NULL AND sku_id='{SKU}' AND quantity=1),
 'active_owned',(SELECT count(*) FROM r WHERE phase='ready' AND state='active' AND owner_type='order' AND owner_id=order_id AND holder_hash IS NULL AND sku_id='{SKU}' AND quantity=1),
 'on_hand',(SELECT on_hand FROM app.stock_levels WHERE tenant_id='{TENANT}' AND sku_id='{SKU}'),
 'reserved',(SELECT reserved FROM app.stock_levels WHERE tenant_id='{TENANT}' AND sku_id='{SKU}'),
 'inbox',(SELECT count(*) FROM app.webhook_inbox WHERE tenant_id='{TENANT}' AND provider='fake' AND provider_account_id='c09d-e2e' ),
 'processed',(SELECT count(*) FROM app.webhook_inbox WHERE tenant_id='{TENANT}' AND provider_account_id='c09d-e2e' AND status='processed' AND processed_at IS NOT NULL),
 'pending',(SELECT count(*) FROM app.webhook_inbox WHERE tenant_id='{TENANT}' AND provider_account_id='c09d-e2e' AND status='pending' AND processed_at IS NULL),
 'captured',(SELECT count(*) FROM app.payment_events WHERE tenant_id='{TENANT}' AND provider_account_id='c09d-e2e' AND status='captured' AND applied AND amount_minor=3140 AND currency='USD'),
 'pending_refund',(SELECT count(*) FROM app.payment_events WHERE tenant_id='{TENANT}' AND provider_account_id='c09d-e2e' AND status='refunded' AND NOT applied AND error_code IS NULL),
 'journals',(SELECT count(*) FROM app.ledger_transactions WHERE tenant_id='{TENANT}'),
 'debits',(SELECT sum(amount) FROM e WHERE side='debit'),
 'credits',(SELECT sum(amount) FROM e WHERE side='credit'),
 'cash',(SELECT sum(CASE WHEN side='debit' THEN amount ELSE -amount END) FROM e WHERE code='cash'),
 'paid_outbox',(SELECT count(*) FROM app.outbox_events WHERE tenant_id='{TENANT}' AND event_type='order.paid'));
"""


def verify(row):
    expected = {'sessions': 4, 'paid': 2, 'receipt_only': 2, 'committed_owned': 2,
                'active_owned': 2, 'on_hand': 18, 'reserved': 2, 'inbox': 4,
                'captured': 2, 'pending_refund': 2, 'processed': 2, 'pending': 2, 'journals': 2, 'cash': 6280, 'paid_outbox': 2}
    if (not isinstance(row, dict) or any(row.get(k) != v for k, v in expected.items())
            or not isinstance(row.get('debits'), int) or row['debits'] <= 0
            or row['debits'] != row.get('credits')):
        raise RuntimeError('C09d purchase order/holder-stock/Inbox/ledger witness differs')
    return {'status': 'PASS', 'facts': row}
