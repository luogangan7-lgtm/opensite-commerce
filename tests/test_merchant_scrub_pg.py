#!/usr/bin/env python3
"""CI-only constraint tests of the declared merchant scrub values.

Uses the existing real-command privacy fixture and unchanged PG triggers. Each
case owns a rollback transaction; this is a contract probe, never a runtime
merchant erasure engine. Called by the foundation PG privacy step.
"""
import importlib.util
import json
import os
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("privacy_registry", ROOT / "scripts/privacy_registry.py")
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)
T = "f0c10000-0000-4000-8000-000000000001"


def expression(policy, column):
    """Test interpreter for the three reviewed declaration strategies only."""
    def value_sql(value):
        if isinstance(value, dict) and set(value) == {"column"}:
            if value["column"] not in {"shipping_minor", "currency"}:
                raise AssertionError("unreviewed template source")
            return '"' + value["column"] + '"'
        if isinstance(value, str):
            return "replace(" + gate.sql_literal(value) + ",'{id}',id::text)"
        return gate.sql_literal(json.dumps(value)) + "::jsonb"

    strategy, value = policy["strategy"], policy["value"]
    if strategy == "constant":
        out = gate.sql_literal(json.dumps(value)) + "::jsonb" if isinstance(value, dict) else gate.sql_literal(value)
    elif strategy == "row_id_placeholder":
        out = value_sql(value)
    elif strategy == "row_json_template":
        out = "jsonb_build_object(" + ",".join(gate.sql_literal(k) + "," + value_sql(v) for k, v in value.items()) + ")"
    else:
        raise AssertionError("unreviewed scrub strategy: " + strategy)
    return f'CASE WHEN "{column}" IS NULL THEN NULL ELSE {out} END' if policy.get("preserve_null") else out


class MerchantScrubPG(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if os.environ.get("GITHUB_ACTIONS") != "true" or not os.environ.get("DATABASE_URL"):
            raise AssertionError("merchant PG probe requires GitHub CI DATABASE_URL")
        cls.registry = gate.load_registry(ROOT)
        # Read declarations without the artifact gate so each DB regression can
        # independently reach its own constraint even while offline tests are RED.
        cls.fields = {key: field["merchant_deletion"] for key, field in cls.registry.items()}
        cls.fixture, _ = gate.render_fixture(cls.registry, (ROOT / "tests/fixtures/privacy_canaries.sql").read_text(), "merchant_probe")

    def run_sql(self, sql):
        gate.psql(ROOT, "BEGIN;\n" + self.fixture + "\n" + sql + "\nROLLBACK;")

    def test_two_variants_keep_unique_keys_and_existing_financial_references(self):
        replacement = expression(self.fields["product_variants.option_key"], "option_key")
        self.run_sql(f"""
INSERT INTO app.product_variants(tenant_id,id,product_id,sku,option_key,lifecycle_status)
VALUES('{T}','f0c10000-0000-4000-8000-000000000031','f0c10000-0000-4000-8000-000000000008','second-sku','second-option','ready');
INSERT INTO app.tenants(id,name,settlement_currency) VALUES('f0c20000-0000-4000-8000-000000000001','other merchant','USD');
INSERT INTO app.products(tenant_id,id,product_code) VALUES('f0c20000-0000-4000-8000-000000000001','f0c20000-0000-4000-8000-000000000008','other-product');
INSERT INTO app.product_variants(tenant_id,id,product_id,sku,option_key)
VALUES('f0c20000-0000-4000-8000-000000000001','f0c20000-0000-4000-8000-000000000009','f0c20000-0000-4000-8000-000000000008','other-sku','other-option');
CREATE TEMP TABLE variants_before AS SELECT tenant_id,id,to_jsonb(v)-'option_key' AS witness FROM app.product_variants v;
CREATE TEMP TABLE references_before AS SELECT 'line' AS kind,to_jsonb(l) AS witness FROM app.order_lines l WHERE tenant_id='{T}'
UNION ALL SELECT 'inventory',to_jsonb(r) FROM app.inventory_reservations r WHERE tenant_id='{T}';
UPDATE app.product_variants SET option_key={replacement} WHERE tenant_id='{T}';
UPDATE app.product_variants SET option_key={replacement} WHERE tenant_id='{T}';
DO $probe$ BEGIN
 IF (SELECT count(DISTINCT option_key) FROM app.product_variants WHERE tenant_id='{T}')<>2
 OR EXISTS(SELECT FROM app.product_variants WHERE tenant_id='{T}' AND option_key<>'erased:'||id::text)
 OR EXISTS(SELECT FROM app.product_variants v JOIN variants_before b USING(tenant_id,id) WHERE to_jsonb(v)-'option_key' IS DISTINCT FROM b.witness)
 OR (SELECT option_key FROM app.product_variants WHERE tenant_id='f0c20000-0000-4000-8000-000000000001')<>'other-option'
 OR EXISTS((SELECT * FROM references_before) EXCEPT (SELECT 'line',to_jsonb(l) FROM app.order_lines l WHERE tenant_id='{T}' UNION ALL SELECT 'inventory',to_jsonb(r) FROM app.inventory_reservations r WHERE tenant_id='{T}'))
 THEN RAISE EXCEPTION 'MERCHANT_VARIANT_WITNESS_CHANGED'; END IF;
END $probe$;
""")

    def test_guest_and_registered_orders_admit_exact_scrub_and_preserve_amounts(self):
        guest = expression(self.fields["orders.guest_contact"], "guest_contact")
        shipping = expression(self.fields["orders.shipping_snapshot"], "shipping_snapshot")
        self.run_sql(f"""
CREATE TEMP TABLE orders_before AS SELECT id,to_jsonb(o)-ARRAY['guest_contact','shipping_snapshot'] AS witness FROM app.orders o WHERE tenant_id='{T}';
SET LOCAL ROLE app_data_rights_executor;
UPDATE app.orders SET guest_contact={guest},shipping_snapshot={shipping} WHERE tenant_id='{T}';
UPDATE app.orders SET guest_contact={guest},shipping_snapshot={shipping} WHERE tenant_id='{T}';
RESET ROLE;
DO $probe$ BEGIN
 IF NOT EXISTS(SELECT FROM app.orders WHERE tenant_id='{T}' AND customer_id IS NULL AND guest_contact->>'email'='erased-'||id::text||'@invalid.test')
 OR NOT EXISTS(SELECT FROM app.orders WHERE tenant_id='{T}' AND customer_id IS NOT NULL AND guest_contact IS NULL)
 OR EXISTS(SELECT FROM app.orders o JOIN orders_before b USING(id) WHERE to_jsonb(o)-ARRAY['guest_contact','shipping_snapshot'] IS DISTINCT FROM b.witness
 OR shipping_snapshot IS DISTINCT FROM jsonb_build_object('erased',true,'amount_minor',shipping_minor,'currency',currency)
 OR guest_contact IS DISTINCT FROM CASE WHEN customer_id IS NULL THEN jsonb_build_object('email','erased-'||id::text||'@invalid.test','erased',true) ELSE NULL END)
 THEN RAISE EXCEPTION 'MERCHANT_ORDER_SCRUB_CHANGED_FINANCIAL_WITNESS'; END IF;
END $probe$;
""")

    def test_journal_scope_needs_no_update_and_guard_still_rejects_update_delete(self):
        policy = self.fields["data_rights_erasures.scope"]
        action = "" if policy["action"] == "retain" else "UPDATE app.data_rights_erasures SET scope=" + expression(policy, "scope") + f" WHERE tenant_id='{T}';"
        self.run_sql(f"""
INSERT INTO app.data_rights_erasures(tenant_id,customer_id,actor_kind,actor_id)
VALUES('{T}','f0c10000-0000-4000-8000-000000000003','service','f0c10000-0000-4000-8000-000000000002');
{action}
DO $probe$ BEGIN
 IF (SELECT scope FROM app.data_rights_erasures WHERE tenant_id='{T}')<>'customer.profile,addresses,credentials,b2b,order_snapshots,exports'
 THEN RAISE EXCEPTION 'MERCHANT_JOURNAL_SCOPE_CHANGED'; END IF;
 BEGIN
  UPDATE app.data_rights_erasures SET scope=scope WHERE tenant_id='{T}';
  RAISE EXCEPTION 'MERCHANT_JOURNAL_UPDATE_ALLOWED';
 EXCEPTION WHEN SQLSTATE '55000' THEN
  IF SQLERRM<>'PRIVACY_ERASURE_JOURNAL_IMMUTABLE' THEN RAISE; END IF;
 END;
 BEGIN
  DELETE FROM app.data_rights_erasures WHERE tenant_id='{T}';
  RAISE EXCEPTION 'MERCHANT_JOURNAL_DELETE_ALLOWED';
 EXCEPTION WHEN SQLSTATE '55000' THEN
  IF SQLERRM<>'PRIVACY_ERASURE_JOURNAL_IMMUTABLE' THEN RAISE; END IF;
 END;
END $probe$;
""")


if __name__ == "__main__":
    unittest.main(verbosity=2)
