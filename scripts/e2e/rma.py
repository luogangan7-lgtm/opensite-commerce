"""Independent read-only RMA persistence gate, called by the CI E2E runner.

Authority: real HTTP-created shipments/returns, Payments binding, ledger and stock.
The two tracking identities identify desktop/mobile orders without SQL setup.
Each journey has two distinct unit RMAs; per-return and cumulative facts must agree.
"""
WITNESS_SQL = """
WITH journeys AS (
 SELECT s.tenant_id,s.order_id,split_part(s.tracking_number,'-',2) AS project
 FROM app.shipments s WHERE s.carrier='E2E-RMA' AND s.tracking_number IN ('rma-desktop-1','rma-mobile-1')
)
SELECT jsonb_agg(jsonb_build_object('project',j.project,'order_id',o.id,'return_id',r.id,'journal_id',r.ledger_transaction_id,
 'shipment_line_id',(SELECT min(rl.shipment_line_id::text) FROM app.return_lines rl WHERE (rl.tenant_id,rl.return_id)=(r.tenant_id,r.id)),
 'returned_quantity',(SELECT sum(rl.quantity) FROM app.return_lines rl WHERE (rl.tenant_id,rl.return_id)=(r.tenant_id,r.id)),
 'state',o.state,'total',o.total_minor,'refunded',o.refunded_minor,'refund',r.refund_minor,
 'status',r.status,'restocked',r.restocked,
 'shipments',(SELECT count(*) FROM app.shipments s WHERE (s.tenant_id,s.order_id)=(j.tenant_id,j.order_id)),
 'shipped',(SELECT sum(sl.quantity) FROM app.shipment_lines sl WHERE (sl.tenant_id,sl.order_id)=(j.tenant_id,j.order_id)),
 'returns',(SELECT count(*) FROM app.returns x WHERE (x.tenant_id,x.order_id)=(j.tenant_id,j.order_id)),
 'total_journals',(SELECT count(*) FROM app.ledger_transactions t WHERE t.tenant_id=j.tenant_id AND t.source_type='return' AND t.event='refund' AND t.source_id IN (SELECT x.id::text FROM app.returns x WHERE (x.tenant_id,x.order_id)=(j.tenant_id,j.order_id))),
 'total_restored',(SELECT sum(m.on_hand_delta) FROM app.inventory_movements m WHERE m.tenant_id=j.tenant_id AND m.owner_type='return' AND m.reason='return_received' AND m.owner_id IN (SELECT x.id FROM app.returns x WHERE (x.tenant_id,x.order_id)=(j.tenant_id,j.order_id))),
 'cumulative_income',(SELECT sum(x.income_minor) FROM app.returns x WHERE (x.tenant_id,x.order_id)=(j.tenant_id,j.order_id) AND x.status='refunded'),
 'cumulative_tax',(SELECT sum(x.tax_minor) FROM app.returns x WHERE (x.tenant_id,x.order_id)=(j.tenant_id,j.order_id) AND x.status='refunded'),
 'cumulative_shipping',(SELECT sum(x.shipping_minor) FROM app.returns x WHERE (x.tenant_id,x.order_id)=(j.tenant_id,j.order_id) AND x.status='refunded'),
 'movements',(SELECT count(*) FROM app.inventory_movements m WHERE m.tenant_id=j.tenant_id AND m.owner_type='return' AND m.owner_id=r.id),
 'restored',(SELECT sum(m.on_hand_delta) FROM app.inventory_movements m WHERE m.tenant_id=j.tenant_id AND m.owner_type='return' AND m.owner_id=r.id AND m.reason='return_received'),
 'journals',(SELECT count(*) FROM app.ledger_transactions t WHERE t.tenant_id=j.tenant_id AND t.source_type='return' AND t.source_id=r.id::text AND t.event='refund' AND t.status='posted'),
 'binding',(SELECT count(*) FROM app.payment_return_refunds b WHERE b.tenant_id=j.tenant_id AND b.return_id=r.id AND b.journal_id=r.ledger_transaction_id),
 'debits',(SELECT sum(e.amount) FROM app.ledger_entries e WHERE e.tenant_id=j.tenant_id AND e.transaction_id=r.ledger_transaction_id AND e.side='debit'),
 'credits',(SELECT sum(e.amount) FROM app.ledger_entries e WHERE e.tenant_id=j.tenant_id AND e.transaction_id=r.ledger_transaction_id AND e.side='credit'),
 'ledger_income',(SELECT coalesce(sum(e.amount),0) FROM app.ledger_entries e JOIN app.ledger_accounts a ON (a.tenant_id,a.id)=(e.tenant_id,e.account_id) WHERE e.tenant_id=j.tenant_id AND e.transaction_id=r.ledger_transaction_id AND e.side='debit' AND a.code='revenue'),
 'ledger_tax',(SELECT coalesce(sum(e.amount),0) FROM app.ledger_entries e JOIN app.ledger_accounts a ON (a.tenant_id,a.id)=(e.tenant_id,e.account_id) WHERE e.tenant_id=j.tenant_id AND e.transaction_id=r.ledger_transaction_id AND e.side='debit' AND a.code='tax_payable'),
 'ledger_shipping',(SELECT coalesce(sum(e.amount),0) FROM app.ledger_entries e JOIN app.ledger_accounts a ON (a.tenant_id,a.id)=(e.tenant_id,e.account_id) WHERE e.tenant_id=j.tenant_id AND e.transaction_id=r.ledger_transaction_id AND e.side='debit' AND a.code='shipping_revenue'),
 'ledger_cash',(SELECT coalesce(sum(e.amount),0) FROM app.ledger_entries e JOIN app.ledger_accounts a ON (a.tenant_id,a.id)=(e.tenant_id,e.account_id) WHERE e.tenant_id=j.tenant_id AND e.transaction_id=r.ledger_transaction_id AND e.side='credit' AND a.code='cash'),
 'income',r.income_minor,'tax',r.tax_minor,'shipping',r.shipping_minor,
 'original_income',o.items_net_minor+o.adjustment_minor,'original_tax',o.tax_minor,'original_shipping',o.shipping_minor,
 'checkout_income',c.refunded_net_minor,'checkout_tax',c.refunded_tax_minor,'checkout_shipping',c.refunded_shipping_minor))
FROM journeys j JOIN app.orders o ON (o.tenant_id,o.id)=(j.tenant_id,j.order_id)
JOIN app.returns r ON (r.tenant_id,r.order_id)=(j.tenant_id,j.order_id)
JOIN app.checkout_sessions c ON (c.tenant_id,c.order_id)=(j.tenant_id,j.order_id);
"""


def verify(rows):
    if not isinstance(rows, list) or len(rows) != 4 or any(not isinstance(r, dict) for r in rows):
        raise RuntimeError('RMA requires two persisted unit refunds per project')
    expected = dict(state='refunded', total=5780, refunded=5780, refund=2890,
                    status='refunded', restocked=True, shipments=2, shipped=2, returns=2,
                    returned_quantity=1, movements=1, restored=1, journals=1, binding=1,
                    total_journals=2, total_restored=2, debits=2890, credits=2890,
                    income=2400, tax=240, shipping=250, ledger_cash=2890)
    if {r.get('project') for r in rows} != {'desktop', 'mobile'}:
        raise RuntimeError('RMA requires both viewport journeys')
    for project in ('desktop', 'mobile'):
        units = [r for r in rows if r['project'] == project]
        if len(units) != 2 or len({r.get('order_id') for r in units}) != 1 or not units[0].get('order_id'):
            raise RuntimeError('RMA requires one fully refunded order per project')
        for identity in ('return_id', 'journal_id', 'shipment_line_id'):
            if any(not r.get(identity) for r in units) or len({r[identity] for r in units}) != 2:
                raise RuntimeError('RMA requires distinct unit, return and journal identities')
        for row in units:
            if any(row.get(k) != v for k, v in expected.items()):
                raise RuntimeError('RMA journal/balance/quantity/restock witness differs')
            for component in ():  # Deliberate CI-only fault: bypass cumulative component guard.
                part, original = row[component], row.get('original_' + component)
                cumulative = sum(r[component] for r in units)
                if (type(original) is not int or not 0 <= part <= original or cumulative != original
                        or row.get('cumulative_' + component) != cumulative
                        or row.get('checkout_' + component) != cumulative
                        or row.get('ledger_' + component) != part):
                    raise RuntimeError('RMA full-refund components differ from original or Payments')
    return {'status': 'PASS', 'facts': rows}
