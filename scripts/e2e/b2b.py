"""CI B2B identity prerequisites and independent read-only persistence witness.

Called by run.py; production APIs own every inquiry/quote/approval/order/payment
write. Seed only company/customer/staff identity only; Fake account mapping is the existing B2C seed.
"""
TENANT = 'c0900001-0000-4000-8000-000000000001'


def seed(psql):
    psql(f"""BEGIN;
    INSERT INTO app.tenant_memberships(tenant_id,user_id,role)
      SELECT '{TENANT}',id,'owner' FROM app.users WHERE email='c24-owner@example.test';
    INSERT INTO app.buyer_companies(tenant_id,id,name)
      SELECT '{TENANT}',('b2b00002-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid,
        'E2E B2B company '||n FROM generate_series(1,4) n;
    INSERT INTO app.customers(tenant_id,id,email,verified_at)
      SELECT '{TENANT}',('b2b00001-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid,
        'b2b-'||CASE WHEN n<=3 THEN 'desktop' ELSE 'mobile' END||'-'||
        CASE (n-1)%3 WHEN 0 THEN 'buyer' WHEN 1 THEN 'approver' ELSE 'outsider' END||'@example.test',
        clock_timestamp() FROM generate_series(1,6) n;
    INSERT INTO app.customer_credentials(tenant_id,customer_id,password_hash)
      SELECT c.tenant_id,c.id,u.password_hash FROM app.customers c CROSS JOIN app.users u
      WHERE c.tenant_id='{TENANT}' AND c.email LIKE 'b2b-%' AND u.email='c24-owner@example.test';
    INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code)
      SELECT '{TENANT}',('b2b00002-0000-4000-8000-'||lpad(
        (CASE WHEN n<=3 THEN 1 ELSE 3 END+CASE WHEN (n-1)%3=2 THEN 1 ELSE 0 END)::text,12,'0'))::uuid,
        ('b2b00001-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid,
        CASE (n-1)%3 WHEN 1 THEN 'approver' ELSE 'purchaser' END FROM generate_series(1,6) n;
    COMMIT;""")


WITNESS_SQL = f"""
SELECT jsonb_agg(jsonb_build_object('project',split_part(u.email,'-',2),
 'approved',q.status='approved' AND a.decision='approved' AND a.requested_by=u.id
    AND a.decided_by<>u.id AND m.role_code='approver' AND m.status='active',
 'immutable',q.version=v.quote_version AND q.snapshot->>'total'='2700'
    AND q.snapshot->>'subtotal'='2000' AND q.snapshot->>'tax_amount'='200'
    AND q.snapshot->>'shipping_amount'='500' AND q.currency='USD'
    AND (SELECT count(*) FROM app.quote_lines l WHERE (l.tenant_id,l.quote_id)=(q.tenant_id,q.id)
      AND l.quantity=2 AND l.unit_price=1000 AND l.line_total=2000)=1,
 'paid',o.state='paid' AND o.sealed AND c.phase='paid' AND c.actor_kind='customer'
    AND c.actor_id=u.id::text AND o.customer_id=u.id AND o.currency=q.currency,
 'total',q.total,'order_total',o.total_minor,
 'approvals',(SELECT count(*) FROM app.quote_approvals WHERE tenant_id=q.tenant_id AND quote_id=q.id),
 'conversions',(SELECT count(*) FROM app.quote_conversions WHERE tenant_id=q.tenant_id AND quote_id=q.id),
 'payments',(SELECT count(*) FROM app.payment_events e JOIN app.webhook_inbox i ON (i.tenant_id,i.id)=(e.tenant_id,e.id)
    WHERE e.tenant_id=o.tenant_id AND e.claimed_order_id=o.id AND e.applied AND e.status='captured'
      AND e.provider='fake' AND e.provider_account_id='c09d-e2e' AND e.provider_ref=c.provider_ref
      AND e.amount_minor=q.total AND e.currency=q.currency AND i.status='processed'),
 'journals',(SELECT count(*) FROM app.ledger_transactions t WHERE t.tenant_id=o.tenant_id AND t.source_id=o.id::text AND t.status='posted'),
 'debits',(SELECT sum(e.amount) FROM app.ledger_entries e JOIN app.ledger_transactions t ON (t.tenant_id,t.id)=(e.tenant_id,e.transaction_id) WHERE t.tenant_id=o.tenant_id AND t.source_id=o.id::text AND e.side='debit'),
 'credits',(SELECT sum(e.amount) FROM app.ledger_entries e JOIN app.ledger_transactions t ON (t.tenant_id,t.id)=(e.tenant_id,e.transaction_id) WHERE t.tenant_id=o.tenant_id AND t.source_id=o.id::text AND e.side='credit')))
FROM app.customers u JOIN app.quotes q ON q.tenant_id=u.tenant_id AND q.accepted_by=u.id
JOIN app.inquiries i ON (i.tenant_id,i.id)=(q.tenant_id,q.inquiry_id) AND i.buyer_email=u.email
JOIN app.quote_approvals a ON (a.tenant_id,a.quote_id)=(q.tenant_id,q.id)
JOIN app.buyer_company_members m ON (m.tenant_id,m.buyer_company_id,m.customer_id)=(a.tenant_id,a.buyer_company_id,a.decided_by)
JOIN app.quote_conversions v ON (v.tenant_id,v.quote_id)=(q.tenant_id,q.id)
JOIN app.orders o ON (o.tenant_id,o.id)=(v.tenant_id,v.order_id)
JOIN app.checkout_sessions c ON (c.tenant_id,c.order_id)=(o.tenant_id,o.id)
WHERE u.tenant_id='{TENANT}' AND u.email IN ('b2b-desktop-buyer@example.test','b2b-mobile-buyer@example.test');
"""


def verify(rows):
    expected = {'approved': True, 'immutable': True, 'paid': True, 'total': 2700,
                'order_total': 2700, 'approvals': 1, 'conversions': 1,
                'payments': 1, 'journals': 1, 'debits': 5400, 'credits': 5400}
    if (not isinstance(rows, list) or len(rows) != 2 or any(not isinstance(r, dict) for r in rows)
            or {r.get('project') for r in rows} != {'desktop', 'mobile'}
            or any(any(type(r.get(k)) is not type(v) or r[k] != v for k, v in expected.items()) for r in rows)):
        raise RuntimeError('B2B approval/immutable quote/order/one payment/balanced ledger witness differs')
    return {'status': 'PASS', 'projects': rows}
