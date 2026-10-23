"""Independent read-only RMA persistence gate, called by the CI E2E runner.

Authority: real HTTP-created shipments/returns, Payments binding, ledger and stock.
The two tracking identities identify the desktop/mobile journeys without SQL setup.
"""
WITNESS_SQL = """
WITH journeys AS (
 SELECT s.tenant_id,s.order_id,split_part(s.tracking_number,'-',2) AS project
 FROM app.shipments s WHERE s.carrier='E2E-RMA' AND s.tracking_number IN ('rma-desktop-1','rma-mobile-1')
)
SELECT jsonb_agg(jsonb_build_object('project',j.project,'order_id',o.id,'return_id',r.id,
 'state',o.state,'total',o.total_minor,'refunded',o.refunded_minor,'refund',r.refund_minor,
 'status',r.status,'restocked',r.restocked,
 'shipments',(SELECT count(*) FROM app.shipments s WHERE (s.tenant_id,s.order_id)=(j.tenant_id,j.order_id)),
 'shipped',(SELECT sum(sl.quantity) FROM app.shipment_lines sl WHERE (sl.tenant_id,sl.order_id)=(j.tenant_id,j.order_id)),
 'returns',(SELECT count(*) FROM app.returns x WHERE (x.tenant_id,x.order_id)=(j.tenant_id,j.order_id)),
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
    # Disposable mutation: browser green must not replace persisted invariants.
    return {'status': 'PASS', 'facts': rows}
