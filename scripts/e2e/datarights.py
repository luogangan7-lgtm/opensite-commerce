"""Read-only privacy journey witnesses; run.py consumes opaque browser identities.

Registry declares columns, explicit joins establish ownership (never email joins).
Only booleans/counts enter receipts. No business state or deadlines are SQL-written.
"""
import json
import re
import uuid


def verify(row):
    if not isinstance(row, dict) or any(row.get(k) != v for k, v in {
            'erased': True, 'auth': 0, 'exports': 2, 'revoked': 2, 'journal': 1,
            'paid': True, 'lines': 1, 'checkout': True, 'attempts': True,
            'notification': True, 'balanced': True, 'cash': 3140}.items()):
        raise RuntimeError('Data-rights erasure/retained money/auth/capability witness differs')
    return row


def witness(psql, root):
    results = []
    for project in ('desktop', 'mobile'):
        data = json.loads((root / f'test-results/e2e/datarights-{project}.json').read_text())
        if data.get('project') != project or len(data.get('exports', [])) != 2:
            raise RuntimeError('Data-rights opaque browser identity inventory differs')
        customer, order = (str(uuid.UUID(data[key])) for key in ('customer', 'order'))
        exports = [str(uuid.UUID(value)) for value in data['exports']]
        tenant = 'c0900001-0000-4000-8000-000000000001'
        row = json.loads(psql(f"""SELECT jsonb_build_object(
          'erased',c.status='disabled' AND c.email='erased+'||c.id||'@erased.invalid' AND c.display_name='[erased]' AND EXISTS(SELECT FROM app.data_rights_erasures WHERE tenant_id=c.tenant_id AND customer_id=c.id),
          'auth',(SELECT count(*) FROM (SELECT customer_id FROM app.customer_credentials WHERE tenant_id=c.tenant_id UNION ALL SELECT customer_id FROM app.customer_sessions WHERE tenant_id=c.tenant_id UNION ALL SELECT customer_id FROM app.customer_auth_tokens WHERE tenant_id=c.tenant_id UNION ALL SELECT customer_id FROM app.customer_addresses WHERE tenant_id=c.tenant_id) a WHERE customer_id=c.id),
          'exports',(SELECT count(*) FROM app.data_rights_exports WHERE tenant_id=c.tenant_id AND customer_id=c.id),
          'revoked',(SELECT count(*) FROM app.data_rights_exports WHERE tenant_id=c.tenant_id AND customer_id=c.id AND id IN('{exports[0]}','{exports[1]}') AND status='revoked' AND token_hash IS NULL AND content_hash IS NULL),
          'paid',o.state='paid' AND o.sealed AND o.total_minor=3140 AND o.currency='USD' AND o.refunded_minor=0,
          'lines',(SELECT count(*) FROM app.order_lines WHERE tenant_id=o.tenant_id AND order_id=o.id AND quantity=1 AND line_total_minor=2640 AND tax_snapshot='{{"erased":true}}'),
          'checkout',(SELECT count(*)=1 AND bool_and(phase='paid' AND amount_minor=3140 AND address_snapshot='{{"erased":true}}' AND start_result='{{"erased":true}}' AND final_result='{{"erased":true}}') FROM app.checkout_sessions WHERE tenant_id=o.tenant_id AND order_id=o.id),
          'attempts',NOT EXISTS(SELECT FROM app.checkout_attempts a JOIN app.checkout_sessions s ON(s.tenant_id,s.id)=(a.tenant_id,a.checkout_id) WHERE s.tenant_id=o.tenant_id AND s.order_id=o.id AND a.result IS DISTINCT FROM '{{"erased":true}}' AND a.result IS NOT NULL),
          'notification',NOT EXISTS(SELECT FROM app.notification_deliveries d WHERE d.tenant_id=o.tenant_id AND d.reference_id=o.id AND (d.recipient<>'erased-'||d.id||'@invalid.test' OR d.status<>'dead_letter' OR d.last_error<>'PRIVACY_ERASED')),
          'journal',(SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=o.tenant_id AND source_id=o.id::text),
          'balanced',(SELECT sum(CASE WHEN e.side='debit' THEN e.amount ELSE -e.amount END)=0 FROM app.ledger_entries e JOIN app.ledger_transactions t ON(t.tenant_id,t.id)=(e.tenant_id,e.transaction_id) WHERE t.tenant_id=o.tenant_id AND t.source_id=o.id::text),
          'cash',(SELECT sum(CASE WHEN e.side='debit' THEN e.amount ELSE -e.amount END) FROM app.ledger_entries e JOIN app.ledger_transactions t ON(t.tenant_id,t.id)=(e.tenant_id,e.transaction_id) JOIN app.ledger_accounts a ON(a.tenant_id,a.id)=(e.tenant_id,e.account_id) WHERE t.tenant_id=o.tenant_id AND t.source_id=o.id::text AND a.code='cash'))
          FROM app.customers c JOIN app.orders o ON(o.tenant_id,o.customer_id)=(c.tenant_id,c.id)
          WHERE c.tenant_id='{tenant}' AND c.id='{customer}' AND o.id='{order}';"""))
        verify(row)
        # Registry-linked personal/contact copies: check every declared column of
        # every verified customer path, including absent address/quote/RMA copies.
        # Presence is reported separately; zero rows never means exercised coverage.
        predicates = {
            'customers': f"r.id='{customer}'",
            'customer_addresses': f"r.customer_id='{customer}'",
            'customer_auth_tokens': f"r.customer_id='{customer}'",
            'orders': f"r.id='{order}'", 'order_lines': f"r.order_id='{order}'",
            'order_adjustments': f"r.order_id='{order}'", 'shipments': f"r.order_id='{order}'",
            'returns': f"r.order_id='{order}'", 'checkout_sessions': f"r.order_id='{order}'",
            'checkout_attempts': f"r.checkout_id IN(SELECT id FROM app.checkout_sessions WHERE tenant_id='{tenant}' AND order_id='{order}')",
            'quotes': f"r.accepted_by='{customer}'",
            'quote_lines': f"r.quote_id IN(SELECT id FROM app.quotes WHERE tenant_id='{tenant}' AND accepted_by='{customer}')",
            'idempotency_records': f"r.actor_kind='customer' AND r.actor_id='{customer}'",
            'inventory_movements': f"r.owner_type='return' AND r.owner_id IN(SELECT id FROM app.returns WHERE tenant_id='{tenant}' AND order_id='{order}')",
            'notification_deliveries': f"r.reference_id='{order}'",
            'outbox_events': f"r.payload->>'order_id'='{order}' OR r.payload->>'notification_id' IN(SELECT id::text FROM app.notification_deliveries WHERE tenant_id='{tenant}' AND reference_id='{order}')",
        }
        copies = []
        for path in sorted((root / 'modules/privacy').glob('*.json')):
            for table, policy in json.loads(path.read_text())['tables'].items():
                fields = []
                for name, spec in policy['columns'].items():
                    identity = spec.get('subject', policy.get('subject', {})).get('identity_path', '')
                    if (spec['classification'] in ('personal', 'contact') and spec['erase']['strategy'] != 'retain'
                            and (any(part in identity for part in ('customers.id', 'orders.customer_id', 'customer_guest_order_links.customer_id'))
                                 or table == 'notification_deliveries')):
                        fields.append(name)
                if not fields:
                    continue
                if table not in predicates or any(not re.fullmatch(r'[a-z_]+', name) for name in [table, *fields]):
                    raise RuntimeError('Data-rights registry ownership coverage changed; review witness joins')
                for field in fields:
                    copy = json.loads(psql(f"""SELECT jsonb_build_object('rows',count(*),'clean',coalesce(bool_and(
                      coalesce((to_jsonb(r)->'{field}')::text,'') NOT LIKE '%e2edr-{project}@example.test%'
                      AND coalesce((to_jsonb(r)->'{field}')::text,'') NOT LIKE '%1 Test Way%'
                      AND coalesce((to_jsonb(r)->'{field}')::text,'') NOT LIKE '%Test City%'),true))
                      FROM app.{table} r WHERE r.tenant_id='{tenant}' AND ({predicates[table]});"""))
                    if copy.get('clean') is not True:
                        raise RuntimeError('Data-rights registered PII remains in ' + table + '.' + field)
                    copies.append({'field': table + '.' + field, **copy})
        if len(copies) < 20:
            raise RuntimeError('Data-rights privacy registry copy inventory unexpectedly shrank')
        results.append({'project': project, 'facts': row, 'registry_copies': copies})
    return {'status': 'PASS', 'projects': results}
