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
    "media_assets.filename", "media_uploads.original_filename", "inventory_movements.reason",
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
{fixture.read_text()}
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
 SELECT string_agg(key||'@'||hits,'; ') INTO leaked FROM pg_temp.privacy_hits() WHERE expect='erase' AND hits IS NOT NULL;
 SELECT string_agg(key,', ') INTO lost FROM pg_temp.privacy_hits() WHERE expect='retain' AND hits IS NULL;
 IF NOT EXISTS(SELECT FROM app.idempotency_records WHERE idempotency_key='staff-unrelated'
  AND response_body->>'snapshot'='privacy-unrelated-staff-control') THEN
  RAISE EXCEPTION 'PRIVACY_GATE erased unrelated staff receipt';
 END IF;
 IF leaked IS NOT NULL THEN RAISE EXCEPTION 'PRIVACY_GATE leftover subject PII: %',leaked; END IF;
 IF lost IS NOT NULL THEN RAISE EXCEPTION 'PRIVACY_GATE erased another subject or retained field: %',lost; END IF;
END $after$;
ROLLBACK;
"""
        result = subprocess.run(["psql", os.environ["DATABASE_URL"], "-X", "-qAt"], input=sql, cwd=ROOT,
                                text=True, capture_output=True, timeout=300)
        # Canary tokens are synthetic; DSNs and credentials are never printed.
        print(result.stdout.strip())
        if result.returncode != 0:
            print(result.stderr.strip(), file=sys.stderr)
            return 1
    print(f"PASS: erasure removed {len(ERASED)} subject canaries and kept {len(RETAINED)} retained/other-subject canaries")
    return 0


if __name__ == "__main__":
    sys.exit(main())
