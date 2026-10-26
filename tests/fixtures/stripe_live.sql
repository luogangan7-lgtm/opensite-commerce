-- Responsibility: CI-only live account/Inbox, never a production migration.
-- Dependencies: disposable migrated PostgreSQL and existing Payments applier.
-- Caller: owner-gated harness and offline fake-Stripe PG regression only.
-- These separate live tables preserve every sandbox/test-pinned constraint.
CREATE SCHEMA stripe_smoke;
REVOKE ALL ON SCHEMA stripe_smoke FROM PUBLIC;
GRANT USAGE ON SCHEMA stripe_smoke TO app_runtime;
CREATE TABLE stripe_smoke.accounts (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 account text NOT NULL CHECK(account ~ '^acct_[A-Za-z0-9_]+$'),
 environment text NOT NULL DEFAULT 'live' CHECK(environment='live'),
 run_id text NOT NULL CHECK(run_id ~ '^[0-9]{1,32}$'),
 PRIMARY KEY(tenant_id,account), UNIQUE(account)
);
CREATE TABLE stripe_smoke.inbox (
 tenant_id uuid NOT NULL,
 account text NOT NULL,
 event_key text NOT NULL CHECK(event_key ~ '^[0-9a-f]{64}$'),
 payload_hash bytea NOT NULL CHECK(octet_length(payload_hash)=32),
 applied boolean NOT NULL DEFAULT false,
 PRIMARY KEY(tenant_id,account,event_key),
 FOREIGN KEY(tenant_id,account) REFERENCES stripe_smoke.accounts(tenant_id,account)
);
ALTER TABLE stripe_smoke.accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE stripe_smoke.accounts FORCE ROW LEVEL SECURITY;
ALTER TABLE stripe_smoke.inbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE stripe_smoke.inbox FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON stripe_smoke.accounts USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY tenant_scope ON stripe_smoke.inbox USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
REVOKE ALL ON stripe_smoke.accounts,stripe_smoke.inbox FROM PUBLIC;
GRANT SELECT ON stripe_smoke.accounts,stripe_smoke.inbox TO app_runtime;
GRANT INSERT(tenant_id,account,event_key,payload_hash),UPDATE(applied) ON stripe_smoke.inbox TO app_runtime;
