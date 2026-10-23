-- stable-id: c19c-refund-facts
-- depends-on: c19b-stripe
-- Responsibility: immutable refund identity and delivery-to-fact bindings.
-- Dependencies: Payments facts and Reliability Inbox; caller: mapped ingress.
-- Legacy facts have no recoverable refund ID: preserve them and quarantine new
-- refund identities on those streams until trusted reconciliation, never guess.
ALTER TABLE app.payment_events ADD COLUMN provider_refund_id text
 CHECK(provider_refund_id IS NULL OR
  (length(provider_refund_id) BETWEEN 1 AND 200 AND provider_refund_id ~ '^[!-~]+$'
   AND status IN('partially_refunded','refunded')));
CREATE UNIQUE INDEX payment_refund_identity ON app.payment_events
 (tenant_id,provider,provider_account_id,environment,provider_refund_id);
GRANT INSERT(provider_refund_id) ON app.payment_events TO app_runtime;

CREATE TABLE app.payment_event_deliveries (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL,
 payment_event_id uuid NOT NULL,
 PRIMARY KEY(tenant_id,id),
 FOREIGN KEY(tenant_id,id) REFERENCES app.webhook_inbox(tenant_id,id),
 FOREIGN KEY(tenant_id,payment_event_id) REFERENCES app.payment_events(tenant_id,id)
);
CREATE INDEX payment_event_delivery_fact ON app.payment_event_deliveries(tenant_id,payment_event_id);
ALTER TABLE app.payment_event_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.payment_event_deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.payment_event_deliveries
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
REVOKE ALL ON app.payment_event_deliveries FROM PUBLIC,app_runtime;
GRANT SELECT ON app.payment_event_deliveries TO app_runtime;
GRANT INSERT(tenant_id,id,payment_event_id) ON app.payment_event_deliveries TO app_runtime;
