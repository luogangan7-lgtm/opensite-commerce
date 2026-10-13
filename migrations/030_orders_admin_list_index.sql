-- Responsibility: index the admin order list keyset (newest first by created_at,id).
-- Depends on 020_orders; used by the orders admin read API (ListAdminOrders).
-- Invariants: index only; no data, grant, policy or snapshot change. A backward
-- scan of (tenant_id,created_at,id) serves ORDER BY created_at DESC,id DESC.
CREATE INDEX orders_admin_list ON app.orders(tenant_id,created_at,id);
