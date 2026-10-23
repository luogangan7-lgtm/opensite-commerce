-- stable-id: c09f-history
-- Responsibility: read verified mailbox order links in customer history.
-- Depends on customer_auth; orders query runner is the only new consumer.
-- Invariant: runtime can read only link identity columns under forced tenant RLS;
-- it cannot create ownership proof, read auth credentials or mutate links.
BEGIN;
GRANT SELECT(tenant_id,customer_id,order_id) ON app.customer_guest_order_links TO app_runtime;
CREATE POLICY customer_history_read ON app.customer_guest_order_links FOR SELECT TO app_runtime
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
COMMIT;
