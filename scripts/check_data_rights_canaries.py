#!/usr/bin/env python3
"""C-22a erasure gate over the C-PII registry canary fixture (GitHub CI only).

Depends on scripts/privacy_registry.py (--emit-fixture/--manifest) and a fully
migrated disposable DATABASE_URL. Called by the foundation workflow after the
registry gate. One transaction: load every declared personal/contact canary,
erase ONLY the fixture's verified customer through app_runtime, then search
every table of every non-system schema (app, queue, public, ...). Subject-owned canaries must be gone;
retained / other-subject / unverified-contact canaries must survive. ROLLBACK.
Invariant: every registry canary is classified below, so a new personal column
fails here until its erase ownership is decided (no email matching, ever).
"""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
CUSTOMER = "f0c10000-0000-4000-8000-000000000003"
# Owned by the fixture customer through a declared identity path.
ERASED = {
    "customers.email", "customers.email_normalized", "customers.display_name",
    "customer_addresses.recipient", "customer_addresses.address_line1", "customer_addresses.address_line2",
    "customer_addresses.city", "customer_addresses.subdivision", "customer_addresses.postal_code",
    "customer_auth_tokens.email_normalized", "notification_deliveries.recipient",
    "orders.shipping_snapshot", "order_lines.tax_snapshot",
    "checkout_sessions.address_snapshot", "checkout_sessions.start_result", "checkout_sessions.final_result",
    "checkout_attempts.result", "quotes.snapshot", "quote_lines.description", "quote_lines.snapshot",
    "idempotency_records.response_body", "idempotency_records.response_body_reference",
    # Guest order linked to the customer by mailbox proof (customer_guest_order_links).
    "orders.guest_contact", "order_adjustments.reason",
    "shipments.carrier", "shipments.tracking_number", "returns.reason",
    "returns.creation_response", "returns.receive_response", "inventory_movements.reason",
}
# Retain strategy, a different subject (staff, merchant, guest, inquiry contact),
# or no verified owner (null_subject_policy): customer erasure must not touch them.
RETAINED = {
    "users.email", "users.email_normalized", "auth_account_attempts.identifier_normalized",
    "auth_login_attempts.identifier_normalized", "auth_login_attempts.ip",
    "company_contacts.contact_value", "company_profile_revisions.profile", "page_revisions.document",
    "blog_post_revisions.title", "blog_post_revisions.excerpt", "blog_post_revisions.body_blocks",
    "site_drafts.title", "site_drafts.description", "site_drafts.navigation", "draft_pages.document",
    "draft_operations.operation", "site_releases.manifest", "site_release_templates.document",
    "inquiries.buyer_email", "inquiries.buyer_name", "inquiries.message",
    "media_assets.filename", "media_uploads.original_filename",
    "audit_events.payload",
    "outbox_events.payload", "outbox_events.last_error", "webhook_inbox.payload_reference",
    "queue.river_job.args", "queue.river_job.errors", "queue.river_job.metadata",
    # Merchant-authored public projection has no verified customer subject.
    "storefront_serving.body", "storefront_serving.fallback_body",
}


def literal(value):
    return "'" + value.replace("'", "''") + "'"


def main():
    if os.environ.get("GITHUB_ACTIONS") != "true" or not os.environ.get("DATABASE_URL"):
        print("FAIL: data-rights canary gate runs only in GitHub CI with DATABASE_URL", file=sys.stderr)
        return 1
    with tempfile.TemporaryDirectory() as tmp:
        fixture, manifest = Path(tmp) / "fixture.sql", Path(tmp) / "canaries.json"
        subprocess.run([sys.executable, "scripts/privacy_registry.py", "--emit-fixture", str(fixture),
                        "--manifest", str(manifest), "--prefix", "privacyerase"], cwd=ROOT, check=True)
        canaries = json.loads(manifest.read_text())
        unclassified = sorted(canaries.keys() - ERASED - RETAINED)
        stale = sorted((ERASED | RETAINED) - canaries.keys())
        if unclassified or stale or ERASED & RETAINED:
            print(f"FAIL: DATA_RIGHTS_CANARY_UNCLASSIFIED {unclassified} stale {stale}", file=sys.stderr)
            return 1
        rows = ",".join(f"({literal(k)},{literal(v)},{literal('erase' if k in ERASED else 'retain')})"
                        for k, v in sorted(canaries.items()))
        sql = f"""\\set ON_ERROR_STOP on
BEGIN;
-- Whole-row text must expose ASCII canaries inside bytea as well as JSON/text.
-- Match privacy_registry.py's escaped-byte representation; data stays unchanged.
SET LOCAL bytea_output='escape';
{fixture.read_text()}
-- Capture every non-PII financial/state/identity cell before controlled erasure.
CREATE TEMP TABLE privacy_fulfillment_before AS
 SELECT 'shipments' tab,id,to_jsonb(s)-ARRAY['carrier','tracking_number'] facts FROM app.shipments s
 UNION ALL SELECT 'returns',id,to_jsonb(r)-ARRAY['reason','creation_response','receive_response'] FROM app.returns r
 UNION ALL SELECT 'inventory_movements',id,to_jsonb(m)-'reason' FROM app.inventory_movements m;
CREATE TEMP TABLE privacy_canary(key text PRIMARY KEY,val text NOT NULL,expect text NOT NULL) ON COMMIT DROP;
INSERT INTO privacy_canary VALUES {rows};
CREATE FUNCTION pg_temp.privacy_hits() RETURNS TABLE(key text,expect text,hits text) LANGUAGE plpgsql AS $f$
DECLARE r record; BEGIN
 CREATE TEMP TABLE IF NOT EXISTS privacy_hit(key text,tab text) ON COMMIT DROP; TRUNCATE privacy_hit;
 FOR r IN SELECT n.nspname s,c.relname t FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE c.relkind IN('r','p','m') AND n.nspname NOT IN('pg_catalog','information_schema') AND NOT starts_with(n.nspname,'pg_') LOOP
  EXECUTE format('INSERT INTO privacy_hit SELECT p.key,%L FROM privacy_canary p WHERE EXISTS(SELECT 1 FROM %I.%I x WHERE strpos(x::text,p.val)>0)',r.s||'.'||r.t,r.s,r.t);
 END LOOP;
 RETURN QUERY SELECT p.key,p.expect,string_agg(h.tab,',' ORDER BY h.tab) FROM privacy_canary p LEFT JOIN privacy_hit h USING(key) GROUP BY p.key,p.expect;
END $f$;
DO $before$ DECLARE missing text; BEGIN
 SELECT string_agg(key,',') INTO missing FROM pg_temp.privacy_hits() WHERE hits IS NULL;
 IF missing IS NOT NULL THEN RAISE EXCEPTION 'DATA_RIGHTS_CANARY_FIXTURE_UNCOVERED %',missing; END IF;
END $before$;
SELECT set_config('app.tenant_id',(SELECT tenant_id::text FROM app.customers WHERE id='{CUSTOMER}'),true);
SET LOCAL ROLE app_runtime;
DO $linked_export$ DECLARE rid uuid:=uuidv7(); payload jsonb; BEGIN
 PERFORM app.data_rights_request_export(rid,'{CUSTOMER}','service',uuidv7());
 payload:=app.data_rights_build_export(rid);
 IF NOT EXISTS(SELECT FROM jsonb_array_elements(payload->'orders') o WHERE o->>'id'='f0c10000-0000-4000-8000-000000000012'
   AND o->'guest_contact'->>'email' IS NOT NULL) THEN
  RAISE EXCEPTION 'PRIVACY_GATE linked guest export omitted'; END IF;
END $linked_export$;
SELECT 'ERASED_COUNTS='||(app.data_rights_erase_customer('{CUSTOMER}','service',uuidv7())->'counts')::text;
RESET ROLE;
DO $linked_future$ DECLARE t uuid:=current_setting('app.tenant_id')::uuid; eid uuid; BEGIN
 IF EXISTS(SELECT FROM app.customer_guest_order_links WHERE tenant_id=t AND customer_id='{CUSTOMER}') THEN
  RAISE EXCEPTION 'PRIVACY_GATE verified links retained'; END IF;
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
 VALUES(t,'order.fulfilled',jsonb_build_object('order_id','f0c10000-0000-4000-8000-000000000012'),
  'orders.events','privacy-linked-guest-after-erasure') RETURNING id INTO eid;
 IF NOT EXISTS(SELECT FROM app.notification_deliveries WHERE tenant_id=t AND source_event_id=eid
  AND status='dead_letter' AND last_error='PRIVACY_ERASED' AND recipient='erased-'||id::text||'@invalid.test') THEN
  RAISE EXCEPTION 'PRIVACY_GATE linked guest future fanout revived'; END IF;
END $linked_future$;
DO $after$ DECLARE leaked text; lost text; BEGIN
 IF EXISTS(SELECT FROM privacy_fulfillment_before b LEFT JOIN (
  SELECT 'shipments' tab,id,to_jsonb(s)-ARRAY['carrier','tracking_number'] facts FROM app.shipments s
  UNION ALL SELECT 'returns',id,to_jsonb(r)-ARRAY['reason','creation_response','receive_response'] FROM app.returns r
  UNION ALL SELECT 'inventory_movements',id,to_jsonb(m)-'reason' FROM app.inventory_movements m
 ) a USING(tab,id) WHERE a.facts IS DISTINCT FROM b.facts) THEN
  RAISE EXCEPTION 'PRIVACY_GATE fulfillment financial/state/identity cells changed'; END IF;
 SELECT string_agg(key||'@'||hits,'; ') INTO leaked FROM pg_temp.privacy_hits() WHERE expect='erase' AND hits IS NOT NULL;
 SELECT string_agg(key,', ') INTO lost FROM pg_temp.privacy_hits() WHERE expect='retain' AND hits IS NULL;
 IF NOT EXISTS(SELECT FROM app.idempotency_records WHERE idempotency_key='staff-unrelated'
  AND response_body->>'snapshot'='privacy-unrelated-staff-control') THEN
  RAISE EXCEPTION 'PRIVACY_GATE erased unrelated staff receipt';
 END IF;
 IF leaked IS NOT NULL THEN RAISE EXCEPTION 'PRIVACY_GATE leftover subject PII: %',leaked; END IF;
 IF lost IS NOT NULL THEN RAISE EXCEPTION 'PRIVACY_GATE erased another subject or retained field: %',lost; END IF;
END $after$;
-- Exercise the real closed recovery path AFTER erasure. No PSP/SMTP IO occurs.
SET LOCAL ROLE app_returns_service;
DO $reconcile$ DECLARE t uuid:=current_setting('app.tenant_id')::uuid;
 rid uuid:=current_setting('privacy.fixture.return_id')::uuid; r jsonb; receipt jsonb; journal uuid;
BEGIN
 r:=app.returns_lock_refund(rid,'privacy-return-refund');
 IF r->>'status'<>'refund_reconcile' OR r->>'provider_ref'<>'opaque-privacy-refund-capture'
  OR r->>'reason'<>'[erased]' THEN RAISE EXCEPTION 'PRIVACY_GATE erased frozen recovery authority'; END IF;
 -- Payments derives principal from the sealed RMA; Returns has no generic posting grant.
 journal:=app.payments_post_return_refund(rid,r->>'refund_key','privacy-refund-after-erasure');
 receipt:=app.returns_complete_refund(rid,r->>'refund_key',r->>'provider_ref',journal);
 IF receipt->>'status'<>'refunded' OR receipt->>'provider_ref'<>r->>'provider_ref' THEN
  RAISE EXCEPTION 'PRIVACY_GATE reconcile after erase failed'; END IF;
 IF app.returns_complete_refund(rid,r->>'refund_key',r->>'provider_ref',journal) IS DISTINCT FROM receipt THEN
  RAISE EXCEPTION 'PRIVACY_GATE reconciliation replay drifted'; END IF;
END $reconcile$;
RESET ROLE;
DO $recovered_sweep$ BEGIN
 IF EXISTS(SELECT FROM pg_temp.privacy_hits() WHERE expect='erase' AND hits IS NOT NULL) THEN
  RAISE EXCEPTION 'PRIVACY_GATE recovery resurrected PII'; END IF;
 IF NOT EXISTS(SELECT FROM app.inventory_movements WHERE reason='privacy-unowned-stock-control') THEN
  RAISE EXCEPTION 'PRIVACY_GATE erased unowned stock text'; END IF;
END $recovered_sweep$;
-- Widen privileges only inside this rolled-back test: rejection must come
-- from the precise scrub guard, rather than an unrelated permission error.
GRANT UPDATE(provider_ref,status) ON app.returns TO app_data_rights_executor;
GRANT UPDATE(quantity) ON app.inventory_movements TO app_data_rights_executor;
SET LOCAL ROLE app_data_rights_executor;
DO $guard_negative$ DECLARE t uuid:=current_setting('app.tenant_id')::uuid;
 rid uuid:=current_setting('privacy.fixture.return_id')::uuid; probe text;
BEGIN
 UPDATE app.returns SET reason='[erased]',creation_response='{{"erased":true}}',receive_response='{{"erased":true}}' WHERE tenant_id=t AND id=rid;
 FOREACH probe IN ARRAY ARRAY['reason','receipt','provider','state','shipment','movement','quantity'] LOOP
  BEGIN
   CASE probe
    WHEN 'reason' THEN UPDATE app.returns SET reason='forged-contact' WHERE tenant_id=t AND id=rid;
    WHEN 'receipt' THEN UPDATE app.returns SET creation_response='{{"contact":"forged"}}' WHERE tenant_id=t AND id=rid;
    WHEN 'provider' THEN UPDATE app.returns SET provider_ref='forged' WHERE tenant_id=t AND id=rid;
    WHEN 'state' THEN UPDATE app.returns SET status='refund_failed' WHERE tenant_id=t AND id=rid;
    WHEN 'shipment' THEN UPDATE app.shipments SET tracking_number='forged' WHERE tenant_id=t;
    WHEN 'movement' THEN UPDATE app.inventory_movements SET reason='forged' WHERE tenant_id=t AND owner_type='return' AND owner_id=rid;
    WHEN 'quantity' THEN UPDATE app.inventory_movements SET quantity=quantity+1 WHERE tenant_id=t AND owner_type='return' AND owner_id=rid;
   END CASE;
   RAISE EXCEPTION 'PRIVACY_GATE scrub guard admitted %',probe;
  EXCEPTION WHEN object_not_in_prerequisite_state THEN
   IF SQLERRM<>'PRIVACY_SCRUB_ONLY' THEN RAISE; END IF;
  END;
 END LOOP;
END $guard_negative$;
RESET ROLE;
SET LOCAL ROLE app_runtime;
DO $future_fulfillment$ DECLARE t uuid:=current_setting('app.tenant_id')::uuid;
 oid uuid:='f0c10000-0000-4000-8000-000000000023'; lid uuid:='f0c10000-0000-4000-8000-000000000024';
 v bigint; shipment jsonb; sid uuid; rid uuid;
BEGIN
 SELECT version INTO STRICT v FROM app.orders WHERE tenant_id=t AND id=oid;
 shipment:=app.fulfillment_create_shipment(oid,v,'privacy-late-shipment','privacy-future-carrier','privacy-future-tracking',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',1)));
 IF shipment->>'carrier'<>'[erased]' OR shipment->>'tracking_number'<>'[erased]' THEN
  RAISE EXCEPTION 'PRIVACY_GATE future shipment resurrected PII'; END IF;
 SELECT id INTO STRICT sid FROM app.shipment_lines WHERE tenant_id=t AND shipment_id=(shipment->>'id')::uuid;
 SELECT version INTO STRICT v FROM app.orders WHERE tenant_id=t AND id=oid;
 PERFORM app.returns_create(oid,v,'privacy-late-return','shipment_return','privacy-future-customer-text',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'shipment_line_id',sid,'quantity',1)));
 SELECT id INTO STRICT rid FROM app.returns WHERE tenant_id=t AND creation_key='privacy-late-return';
 SELECT version INTO STRICT v FROM app.orders WHERE tenant_id=t AND id=oid;
 PERFORM app.returns_receive(rid,v,'privacy-late-receive',true);
 IF NOT EXISTS(SELECT FROM app.returns WHERE tenant_id=t AND id=rid AND reason='[erased]'
  AND creation_response='{{"erased":true}}'::jsonb AND receive_response='{{"erased":true}}'::jsonb) THEN
  RAISE EXCEPTION 'PRIVACY_GATE future return resurrected PII'; END IF;
END $future_fulfillment$;
RESET ROLE;
ROLLBACK;
"""
        result = subprocess.run(["psql", os.environ["DATABASE_URL"], "-X", "-qAt"], input=sql, cwd=ROOT,
                                text=True, capture_output=True, timeout=300)
        # Canary tokens are synthetic; DSNs and credentials are never printed.
        print(result.stdout.strip())
        if result.returncode != 0:
            print(result.stderr.strip(), file=sys.stderr)
            return 1
        # Each injected defect lives in its own disposable transaction. A failed
        # assertion must identify that defect; unrelated fixture failures do not
        # count as RED. psql disconnect rolls back the failed transaction.
        mutations = (
            ("exact scrub", "SET LOCAL ROLE app_data_rights_executor;\nDO $guard_negative$",
             "CREATE OR REPLACE FUNCTION app.data_rights_scrub_guard() RETURNS trigger "
             "LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp "
             "AS $mutant$ BEGIN RETURN NEW; END $mutant$;\n",
             "PRIVACY_GATE scrub guard admitted reason"),
            ("future fulfillment", "SET LOCAL ROLE app_runtime;\nDO $future_fulfillment$",
             "DROP TRIGGER data_rights_fulfillment_erased_guard ON app.shipments;\n",
             "PRIVACY_GATE future shipment resurrected PII"),
        )
        for name, anchor, fault, marker in mutations:
            if sql.count(anchor) != 1:
                print(f"FAIL: mutation anchor missing for {name}", file=sys.stderr)
                return 1
            mutant = subprocess.run(["psql", os.environ["DATABASE_URL"], "-X", "-qAt"],
                                    input=sql.replace(anchor, fault + anchor), cwd=ROOT,
                                    text=True, capture_output=True, timeout=300)
            if mutant.returncode == 0 or marker not in mutant.stderr:
                print(f"FAIL: {name} mutation did not reach its expected RED assertion", file=sys.stderr)
                print(mutant.stderr.strip(), file=sys.stderr)
                return 1
            print(f"PASS: {name} control GREEN / injected defect RED ({marker})")
    print(f"PASS: erasure removed {len(ERASED)} subject canaries and kept {len(RETAINED)} retained/other-subject canaries")
    return 0


if __name__ == "__main__":
    sys.exit(main())
