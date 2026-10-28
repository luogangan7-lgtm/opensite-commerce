-- stable-id: mdel00-caller-keys
-- depends-on: c19d-identity
-- depends-on: c22a-data-rights
-- depends-on: c18b-quote-conversion
-- Responsibility: database admission for caller identities, including sqlc/copy/async writes.
-- Dependencies: existing caller-key tables; no new owners, grants, states or triggers.
-- Callers: every table writer. Invariant: caller keys contain lowercase UUIDs only.
-- Compatibility: reject the migration if any existing key/scope violates the contract;
-- do not rewrite immutable receipts, financial identities or historical caller text.
-- Rollback before integration: remove this unlanded proposal; after landing: forward repair.
ALTER TABLE app.idempotency_records ADD CONSTRAINT idempotency_records_idempotency_key_uuid CHECK (idempotency_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');
ALTER TABLE app.inventory_reservations ADD CONSTRAINT inventory_reservations_idempotency_key_uuid CHECK (idempotency_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');
ALTER TABLE app.orders ADD CONSTRAINT orders_creation_key_uuid CHECK (creation_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');
ALTER TABLE app.shipments ADD CONSTRAINT shipments_creation_key_uuid CHECK (creation_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');
ALTER TABLE app.returns ADD CONSTRAINT returns_creation_key_uuid CHECK (creation_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');
ALTER TABLE app.returns ADD CONSTRAINT returns_receive_key_uuid CHECK (receive_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');
ALTER TABLE app.returns ADD CONSTRAINT returns_refund_key_uuid CHECK (refund_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');
ALTER TABLE app.payment_return_refunds ADD CONSTRAINT payment_return_refunds_attempt_key_uuid CHECK (attempt_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');
ALTER TABLE app.payment_refund_attempts ADD CONSTRAINT payment_refund_attempts_attempt_key_uuid CHECK (attempt_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');
ALTER TABLE app.checkout_sessions ADD CONSTRAINT checkout_sessions_request_key_uuid CHECK (request_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');
ALTER TABLE app.checkout_sessions ADD CONSTRAINT checkout_sessions_claim_key_uuid CHECK (claim_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');
ALTER TABLE app.checkout_attempts ADD CONSTRAINT checkout_attempts_event_key_uuid CHECK (event_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');
ALTER TABLE app.notification_redrive_requests ADD CONSTRAINT notification_redrive_requests_idempotency_key_uuid CHECK (idempotency_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');
ALTER TABLE app.quote_conversions ADD CONSTRAINT quote_conversions_idempotency_key_uuid CHECK (idempotency_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');
ALTER TABLE app.data_rights_erasures ADD CONSTRAINT data_rights_erasures_scope_enum CHECK (scope = 'customer.profile,addresses,credentials,b2b,order_snapshots,exports');
