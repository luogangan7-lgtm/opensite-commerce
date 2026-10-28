#!/usr/bin/env bash
# Upgrade gates preserve every shipped app module plus separate River history; real PG18 only.
# Depends on: a superuser DATABASE_URL (or Docker compose) able to CREATE DATABASE.
# Called by: CI after check_pg.sh. Each case runs in its own throwaway database.
# Invariants: 010 refuses to seal a live release with unsafe media; every pre-010
# release ends up sealed (history is immutable); releases created after 010 are not.
# Reliability preserves compatible pending data and refuses ambiguous old results.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
# shellcheck source=scripts/lib/pg_url.sh
source "$ROOT/scripts/lib/pg_url.sh"
if [[ -n "${DATABASE_URL:-}" ]]; then
  admin() { psql "$DATABASE_URL" -X -q -At -v ON_ERROR_STOP=1 "$@"; }
  # Never keep the caller's query dbname: it overrides the path database.
  db() { local name=$1; shift; psql "$(pg_url_for_database "$DATABASE_URL" "$name")" -X -q -At -v ON_ERROR_STOP=1 "$@"; }
else
  admin() { docker compose -f deploy/compose.yaml exec -T postgres psql -X -q -At -U postgres -d opensite -v ON_ERROR_STOP=1 "$@"; }
  db() { local name=$1; shift; docker compose -f deploy/compose.yaml exec -T postgres psql -X -q -At -U postgres -d "$name" -v ON_ERROR_STOP=1 "$@"; }
fi

fresh_009() {  # $1 = database name; applies 001-009 exactly as shipped
  admin -c "DROP DATABASE IF EXISTS $1" -c "CREATE DATABASE $1" 2>/dev/null
  local actual
  actual="$(db "$1" -c 'SELECT current_database()')"
  [[ $actual == "$1" ]] || { echo "FAIL: upgrade target is $actual, not $1 (dbname override)" >&2; exit 1; }
  for f in migrations/00[1-9]_*.sql; do db "$1" < "$f" >/dev/null; done
}
apply_010() { { echo 'BEGIN;'; cat migrations/010_security_hardening.sql; echo 'COMMIT;'; } | db "$1" 2>&1 >/dev/null; }

# Upstream River history stays separate from app migration checksums.
river_db() {
 local name=$1; shift
 if [[ -n "${DATABASE_URL:-}" ]]; then
  local river_url
  river_url="$(pg_url_for_database "$DATABASE_URL" "$name")"
  DATABASE_URL="$river_url" ./scripts/migrate_river.sh "$@"
 else
  RIVER_DATABASE_NAME="$name" ./scripts/migrate_river.sh "$@"
 fi
}


# Filename suffix, not an integration-assigned number, identifies app queue ACL.
# Every predecessor/successor loop goes through this guard before app SQL runs.
apply_forward() {
 local name=$1 file=$2
 if [[ $file == *_queue_authorization.sql ]]; then river_db "$name"; fi
 { echo 'BEGIN;'; cat "$file"; echo 'COMMIT;'; } | db "$name" >/dev/null
}

SEED_SITE=$(cat <<'SQL'
INSERT INTO app.tenants(name) VALUES('upgrade') RETURNING id AS t \gset
INSERT INTO app.organizations(tenant_id,display_name) VALUES(:'t','U') RETURNING id AS o \gset
INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(:'t',:'o','U') RETURNING id AS s \gset
INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes,status)
 VALUES(:'t','local','ready',sha256('ready'::bytea),1,'ready') RETURNING id AS b \gset
INSERT INTO app.media_assets(tenant_id,blob_id,filename,mime_type,visibility)
 VALUES(:'t',:'b','pub','image/png','public') RETURNING id AS a_pub \gset
INSERT INTO app.media_assets(tenant_id,blob_id,filename,mime_type)
 VALUES(:'t',:'b','priv','image/png') RETURNING id AS a_priv \gset
INSERT INTO app.pages(tenant_id,site_id,page_key) VALUES(:'t',:'s','home') RETURNING id AS pg \gset
INSERT INTO app.page_revisions(tenant_id,site_id,page_id,locale,revision,document) VALUES(:'t',:'s',:'pg','en',1,'{}');
SQL
)
release() {  # psql snippet: create release number $1 as variable :r$1, linking media variable $2
  printf "INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES(:'t',:'s',%s,'u','{}',sha256('r%s'::bytea)) RETURNING id AS r%s \\gset
INSERT INTO app.site_release_media(tenant_id,site_id,release_id,media_id) VALUES(:'t',:'s',:'r%s',:'%s');\n" "$1" "$1" "$1" "$1" "$2"
}

fail=0
pass() { echo "PASS: $*"; }
bad() { echo "FAIL: $*" >&2; fail=1; }

# Case 1: the live release references private media -> 010 refuses and leaves nothing.
DB=upgrade_unsafe_$$
fresh_009 $DB
{ echo "$SEED_SITE"; release 1 a_priv; echo "UPDATE app.sites SET published_release_id=:'r1' WHERE id=:'s';"; } | db $DB >/dev/null
if out=$(apply_010 $DB); then bad "010 sealed a live release with private media"
elif [[ $out != *PUBLISHED_RELEASE_MEDIA_NOT_PUBLIC_READY* ]]; then bad "010 failed for another reason: $out"
elif [[ $(db $DB -c "SELECT count(*) FROM pg_attribute WHERE attrelid='app.site_releases'::regclass AND attname='sealed'") != 0 ]]; then bad "refused 010 left schema behind"
else pass "upgrade refuses a live release with unsafe media and rolls back"; fi
admin -c "DROP DATABASE $DB"

# Case 2: R1 published then replaced by R2, R3 an in-flight candidate. All pre-010
# releases must be sealed (children rejected); a release created after 010 is not.
DB=upgrade_history_$$
fresh_009 $DB
{ echo "$SEED_SITE"; release 1 a_pub; release 2 a_pub; release 3 a_priv
  echo "UPDATE app.sites SET published_release_id=:'r1' WHERE id=:'s';"
  echo "UPDATE app.sites SET published_release_id=:'r2' WHERE id=:'s';"; } | db $DB >/dev/null
if ! out=$(apply_010 $DB); then bad "010 failed on safe history: $out"; else
  result=$(db $DB 2>&1 <<'SQL'
SELECT set_config('app.tenant_id',(SELECT id::text FROM app.tenants WHERE name='upgrade'),false);
SET ROLE app_runtime;
DO $check$
DECLARE t uuid:=current_setting('app.tenant_id')::uuid; s uuid; pg uuid; a uuid; r record; fresh uuid; n int:=0;
BEGIN
 SELECT id INTO STRICT s FROM app.sites WHERE tenant_id=t;
 SELECT id INTO STRICT pg FROM app.pages WHERE tenant_id=t;
 FOR r IN SELECT id,release_no FROM app.site_releases WHERE tenant_id=t ORDER BY release_no LOOP
  BEGIN
   INSERT INTO app.site_release_routes(tenant_id,site_id,release_id,locale,path,page_id,page_revision)
   VALUES(t,s,r.id,'en','/late',pg,1);
   RAISE EXCEPTION 'UPGRADE_GATE route appended to pre-010 release %',r.release_no;
  EXCEPTION WHEN object_not_in_prerequisite_state THEN n:=n+1; END;
  BEGIN
   INSERT INTO app.site_release_templates(tenant_id,site_id,release_id,locale,template_kind,document)
   VALUES(t,s,r.id,'en','product','{}');
   RAISE EXCEPTION 'UPGRADE_GATE template appended to pre-010 release %',r.release_no;
  EXCEPTION WHEN object_not_in_prerequisite_state THEN n:=n+1; END;
  SELECT id INTO STRICT a FROM app.media_assets WHERE tenant_id=t AND id NOT IN
   (SELECT media_id FROM app.site_release_media WHERE tenant_id=t AND release_id=r.id) LIMIT 1;
  BEGIN
   INSERT INTO app.site_release_media(tenant_id,site_id,release_id,media_id) VALUES(t,s,r.id,a);
   RAISE EXCEPTION 'UPGRADE_GATE media appended to pre-010 release %',r.release_no;
  EXCEPTION WHEN object_not_in_prerequisite_state THEN n:=n+1; END;
 END LOOP;
 INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES(t,s,4,'u','{}',sha256('r4'::bytea)) RETURNING id INTO fresh;
 INSERT INTO app.site_release_routes(tenant_id,site_id,release_id,locale,path,page_id,page_revision)
 VALUES(t,s,fresh,'en','/new',pg,1);
 RAISE NOTICE 'UPGRADE_GATE_REJECTED=%',n;
END $check$;
RESET ROLE;
SELECT string_agg(release_no||':'||sealed,',' ORDER BY release_no) FROM app.site_releases;
SQL
  ) || true
  if [[ $result == *"UPGRADE_GATE_REJECTED=9"* && $result == *"1:true,2:true,3:true,4:false"* ]]; then
    pass "upgrade seals replaced history and candidates (9/9 appends rejected); post-010 release appendable"
  else bad "history upgrade: $result"; fi
fi
admin -c "DROP DATABASE $DB"

# Reliability proposals are located by logical suffix, independent of numbering.
# Test compatible pending data and fail-closed legacy states without rewriting 007.
shopt -s nullglob
reliability=(migrations/[0-9][0-9][0-9]_platform_reliability.sql)
[[ ${#reliability[@]} == 1 ]] || { echo 'FAIL: exactly one reliability migration required' >&2; exit 1; }
fresh_before_reliability() {
 admin -c "DROP DATABASE IF EXISTS $1" -c "CREATE DATABASE $1" 2>/dev/null
 local f
 for f in migrations/[0-9][0-9][0-9]_*.sql; do
  [[ $f < ${reliability[0]} ]] || break
  apply_forward "$1" "$f"
 done
}
apply_reliability() { { echo 'BEGIN;'; cat "${reliability[0]}"; echo 'COMMIT;'; } | db "$1" 2>&1 >/dev/null; }
RELIABILITY_DB=""
SHIPPING_DB=""
TAX_DB=""
CUSTOMERS_DB=""
LEDGER_DB=""
ORDERS_DB=""
STOREFRONT_DB=""
NOTIFICATIONS_DB=""
QUOTES_DB=""
CMS_DB=""
DATA_RIGHTS_DB=""
CATALOG_DB=""
CART_DB=""
QUEUE_DB=""
LIMITS_DB=""
OPS_METRICS_DB=""
INVENTORY_DB=""
INVENTORY_OWNER_DB=""
PRICING_DB=""
LISTING_DB=""
IDENTITY_DB=""
LOGIN_DB=""
CHECKOUT_DB=""
SERVING_DB=""
CUSTOMER_AUTH_DB=""
PAYMENTS_DB=""
FULFILLMENT_DB=""
CONVERSION_DB=""
CLOCK_DB=""
RECOVERY_DB=""
PRIVACY_HARDENING_DB=""
AUDIT_DB=""
CALLER_KEYS_DB=""
LIFECYCLE_DB=""
IDENTITY_MERCHANT_DB=""
cleanup_forward_databases() {
 # Attempt each owned database independently even when an earlier DROP fails.
 local name
 for name in "$RELIABILITY_DB" "$SHIPPING_DB" "$TAX_DB" "$CUSTOMERS_DB" "$LEDGER_DB" "$CATALOG_DB" "$ORDERS_DB" "$QUEUE_DB" "$LIMITS_DB" "$INVENTORY_DB" "$INVENTORY_OWNER_DB" "$PRICING_DB" "$LISTING_DB" "$CART_DB" "$STOREFRONT_DB" "$OPS_METRICS_DB" "$NOTIFICATIONS_DB" "$QUOTES_DB" "$IDENTITY_DB" "$LOGIN_DB" "$CMS_DB" "$CHECKOUT_DB" "$CUSTOMER_AUTH_DB" "$SERVING_DB" "$PAYMENTS_DB" "$DATA_RIGHTS_DB" "$FULFILLMENT_DB" "$CONVERSION_DB" "$CLOCK_DB" "$RECOVERY_DB" "$PRIVACY_HARDENING_DB" "$AUDIT_DB" "$CALLER_KEYS_DB" "$LIFECYCLE_DB" "$IDENTITY_MERCHANT_DB"; do
  [[ -z $name ]] || admin -c "DROP DATABASE IF EXISTS $name" >/dev/null 2>&1 || true
 done
}
trap cleanup_forward_databases EXIT
RELIABILITY_DB=reliability_upgrade_$$
fresh_before_reliability "$RELIABILITY_DB"
db "$RELIABILITY_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('legacy-reliability') RETURNING id AS t \gset
INSERT INTO app.outbox_events(tenant_id,event_type,payload) VALUES(:'t','legacy','{}');
INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at)
 VALUES(:'t','service',uuidv7(),'legacy','ac585171-8d9c-5d11-8feb-a32bc62aa7ad',sha256('request'::bytea),now()+interval '1 day');
SQL
if ! out=$(apply_reliability "$RELIABILITY_DB"); then bad "pending reliability upgrade: $out"
elif [[ $(db "$RELIABILITY_DB" -c "SELECT count(*) FROM app.outbox_events WHERE delivery_target='legacy' AND dedup_key=id::text AND status='pending' AND lease_owner IS NULL AND attempts=0") != 1 ]]; then bad 'legacy pending event identity changed'
elif [[ $(db "$RELIABILITY_DB" -c "SELECT count(*) FROM app.idempotency_records WHERE state='in_progress' AND claim_token IS NOT NULL AND request_hash=sha256('request'::bytea) AND response_body_reference IS NULL") != 1 ]]; then bad 'legacy in-progress request identity changed'
elif ! db "$RELIABILITY_DB" < tests/db_reliability.sql >/dev/null; then bad 'reliability SQL gate after pending-data upgrade'
else pass 'reliability upgrade preserves pending events/requests and all new invariants'; fi
admin -c "DROP DATABASE $RELIABILITY_DB"; RELIABILITY_DB=""

for legacy_state in processing delivered failed completed; do
 RELIABILITY_DB=reliability_refuse_$$
 fresh_before_reliability "$RELIABILITY_DB"
 db "$RELIABILITY_DB" -c "INSERT INTO app.tenants(name) VALUES('legacy-reconcile')" >/dev/null
 if [[ $legacy_state == processing || $legacy_state == delivered ]]; then
  db "$RELIABILITY_DB" -c "INSERT INTO app.outbox_events(tenant_id,event_type,payload,status) SELECT id,'legacy','{}','$legacy_state' FROM app.tenants" >/dev/null
 else
  db "$RELIABILITY_DB" -c "INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,state,response_status,response_body) SELECT id,'service',uuidv7(),'legacy','ac585171-8d9c-5d11-8feb-a32bc62aa7ad',sha256('request'::bytea),now()+interval '1 day','$legacy_state',201,'{\"legacy\":true}' FROM app.tenants" >/dev/null
 fi
 if out=$(apply_reliability "$RELIABILITY_DB"); then bad "reliability silently accepted legacy $legacy_state"
 elif [[ $out != *RELIABILITY_UPGRADE_REQUIRES_RECONCILIATION* ]]; then bad "reliability $legacy_state failed for another reason: $out"
 elif [[ $(db "$RELIABILITY_DB" -c "SELECT to_regclass('app.webhook_inbox') IS NULL AND NOT EXISTS(SELECT FROM pg_attribute WHERE attrelid='app.outbox_events'::regclass AND attname='lease_owner' AND NOT attisdropped)") != t ]]; then bad "refused reliability $legacy_state left schema behind"
 else pass "reliability refuses ambiguous legacy $legacy_state atomically"; fi
 admin -c "DROP DATABASE $RELIABILITY_DB"; RELIABILITY_DB=""
done
[[ $fail -eq 0 ]] || exit 1
# C-11a: install additive inventory over preexisting tenant/catalog data. Discover
# the module file by suffix, so integration renumbering does not alter this gate.
inventory_files=(migrations/[0-9][0-9][0-9]_inventory.sql)
[[ ${#inventory_files[@]} == 1 ]] || exit 1
DB=upgrade_inventory_$$; INVENTORY_DB=$DB
fresh_009 $DB
# Only predecessors; later authorization depends on inventory routines.
for f in migrations/0[1-9][0-9]_*.sql; do
  [[ $f < ${inventory_files[0]} ]] || break
  apply_forward "$DB" "$f"
done
# No inventory tables existed when this tenant and SKU were written.
db $DB <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('inventory-upgrade') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','existing-product') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 VALUES(:'t',:'p','existing-sku','default');
SQL
{ echo 'BEGIN;'; cat "${inventory_files[0]}"; echo 'COMMIT;'; } | db $DB >/dev/null
result=$(db $DB <<'SQL'
SELECT set_config('app.tenant_id',(SELECT id::text FROM app.tenants WHERE name='inventory-upgrade'),false);
SELECT set_config('upgrade.sku',(SELECT id::text FROM app.product_variants WHERE sku='existing-sku'),false);
SET ROLE app_inventory_manager;
SELECT app.inventory_adjust(current_setting('upgrade.sku')::uuid,2,'existing SKU receipt',uuidv7(),'inbound');
RESET ROLE;
SET ROLE app_runtime;
SELECT (app.inventory_reserve(current_setting('upgrade.sku')::uuid,1,'checkout',uuidv7(),'afff0635-f78d-5d89-91d7-299cff38235e',900)).id AS rid \gset
SELECT app.inventory_commit(:'rid');
SELECT 'INVENTORY_UPGRADE='||s.on_hand||'|'||s.reserved||'|'||
 (SELECT count(*) FROM app.inventory_movements)||'|'||
 (SELECT count(*) FROM app.products WHERE product_code='existing-product')||'|'||
 (SELECT count(*) FROM app.product_variants WHERE sku='existing-sku')
 FROM app.stock_levels s;
SQL
)
if [[ $result == *'INVENTORY_UPGRADE=1|0|3|1|1'* ]]; then
  pass 'inventory upgrade preserves existing tenant/catalog and commits with bounded counters +3 movements'
else bad "inventory upgrade: $result"; fi
# Install every successor before the exact full-schema matrix.
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${inventory_files[0]} ]] || continue
 apply_forward "$DB" "$f"
done
river_db "$DB" --verify
# Full scoped SQL and exact authority matrix also run against the upgraded DB.
db $DB -f tests/db_inventory.sql >/dev/null
db $DB -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $DB"

# Existing catalog rows from 001..010 remain usable by the forward pricing DDL.
# Apply every post-010 migration by its current filename, not a reserved number.
DB=upgrade_pricing_$$; PRICING_DB=$DB
fresh_009 $DB
if ! out=$(apply_010 $DB); then bad "pricing base hardening: $out"; else
 db "$DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('pricing-upgrade') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','OLD') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 VALUES(:'t',:'p','OLD','default');
SQL
 for f in migrations/[0-9][0-9][0-9]_*.sql; do
  number=${f##*/}; number=${number%%_*}
  if ((10#$number>10)); then apply_forward "$DB" "$f"; fi
 done
 result=$(db "$DB" <<'SQL'
INSERT INTO app.price_lists(tenant_id,currency,starts_at)
 SELECT id,'USD',now() FROM app.tenants WHERE name='pricing-upgrade' RETURNING id AS l \gset
INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount)
 SELECT tenant_id,:'l',id,1,0 FROM app.product_variants WHERE sku='OLD';
SELECT count(*) FROM app.price_entries e JOIN app.product_variants v
 ON (e.tenant_id,e.sku_id)=(v.tenant_id,v.id) WHERE v.sku='OLD' AND e.unit_amount=0;
SQL
 )
 if [[ $result == 1 ]]; then pass "pricing upgrade preserves existing SKU and exact zero price"
 else bad "pricing upgrade witness: $result"; fi
fi
admin -c "DROP DATABASE $DB"
# INT-15: apply ALL predecessor migrations, then upgrade populated Shipping data.
# Filename ordering is the migration order; no shell octal/numeric conversion.
shipping_files=(migrations/[0-9][0-9][0-9]_shipping.sql)
[[ ${#shipping_files[@]} -eq 1 && -f ${shipping_files[0]} ]] || { echo 'FAIL: exactly one shipping migration required'; exit 1; }
SHIPPING_DB=upgrade_shipping_$$
DB=$SHIPPING_DB
admin -c "DROP DATABASE IF EXISTS $DB" -c "CREATE DATABASE $DB" 2>/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${shipping_files[0]} ]] || break
 apply_forward "$DB" "$f"
done
db "$DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('shipping-upgrade') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','shipping-legacy') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
VALUES(:'t',:'p','shipping-legacy','default');
SQL
{ echo 'BEGIN;'; cat "${shipping_files[0]}"; echo 'COMMIT;'; } | db "$DB" >/dev/null
result=$(db "$DB" <<'SQL'
SELECT id AS t FROM app.tenants WHERE name='shipping-upgrade' \gset
INSERT INTO app.shipping_zones(tenant_id,name) VALUES(:'t','legacy tenant') RETURNING id AS z \gset
INSERT INTO app.shipping_profiles(tenant_id,name,currency) VALUES(:'t','P','USD') RETURNING id AS p \gset
INSERT INTO app.shipping_zone_destinations VALUES(:'t',:'z','US','');
INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,min_weight_g,max_weight_g)
VALUES(:'t',:'p',:'z','by_weight','USD',0,0,500);
BEGIN;
SELECT set_config('app.tenant_id',:'t',true);
SET LOCAL ROLE app_runtime;
SELECT 'SHIPPING_UPGRADE='||(SELECT count(*) FROM app.shipping_rates WHERE amount=0)||'|'||
 (SELECT count(*) FROM app.products WHERE product_code='shipping-legacy')||'|'||
 (SELECT count(*) FROM app.product_variants WHERE sku='shipping-legacy');
COMMIT;
SQL
)
if [[ $result == *'SHIPPING_UPGRADE=1|1|1'* ]]; then pass "shipping upgrade preserves existing tenant/catalog; runtime reads zero-cost tier"
else bad "shipping upgrade witness: $result"; fi
# Include any later modules before validating the exact whole-schema matrix.
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${shipping_files[0]} ]] || continue
 apply_forward "$DB" "$f"
done
db "$DB" -f tests/db_shipping.sql >/dev/null
db "$DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $DB"
SHIPPING_DB=""

# Tax upgrade: apply ALL predecessors in filename order, then seed real legacy
# tenant/catalog data before applying the logical tax module (number may change).
tax=(migrations/[0-9][0-9][0-9]_tax.sql)
[[ ${#tax[@]} == 1 ]] || { echo 'FAIL: exactly one tax migration required' >&2; exit 1; }
TAX_DB=tax_upgrade_$$
admin -c "CREATE DATABASE $TAX_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${tax[0]} ]] || break
 apply_forward "$TAX_DB" "$f"
done
db "$TAX_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('tax-existing-tenant') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code,lifecycle_status) VALUES(:'t','tax-existing-product','draft');
SQL
{ echo 'BEGIN;'; cat "${tax[0]}"; echo 'COMMIT;'; } | db "$TAX_DB" >/dev/null
# Exercise new settings/rules through runtime and preserve the existing product.
result=$(db "$TAX_DB" <<'SQL'
SELECT set_config('app.tenant_id',(SELECT id::text FROM app.tenants WHERE name='tax-existing-tenant'),false);
SET ROLE app_runtime;
INSERT INTO app.tax_settings(tenant_id,prices_include_tax) VALUES(current_setting('app.tenant_id')::uuid,true);
INSERT INTO app.tax_zones(tenant_id,name) VALUES(current_setting('app.tenant_id')::uuid,'US') RETURNING id AS z \gset
INSERT INTO app.tax_zone_locations(tenant_id,zone_id,country_code) VALUES(current_setting('app.tenant_id')::uuid,:'z','US');
INSERT INTO app.tax_categories(tenant_id,code,name) VALUES(current_setting('app.tenant_id')::uuid,'standard','Standard');
INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from) VALUES(current_setting('app.tenant_id')::uuid,:'z','standard',500,'2026-01-01Z');
SELECT (SELECT count(*) FROM app.products WHERE product_code='tax-existing-product')||'|'||
 (SELECT prices_include_tax FROM app.tax_settings)||'|'||(SELECT rate_bp FROM app.tax_rates);
SQL
)
[[ $result == *'1|true|500'* ]] || bad "tax populated upgrade witness: $result"
# Apply successors too: the complete-schema exact matrix must remain meaningful
# when integration later puts another module after Tax.
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${tax[0]} ]] || continue
 apply_forward "$TAX_DB" "$f"
done
if ! db "$TAX_DB" < tests/db_tax.sql >/dev/null || ! db "$TAX_DB" < tests/db_role_matrix.sql >/dev/null; then
 bad 'tax constraints/role matrix after populated upgrade'
else pass 'tax upgrade preserves prior catalog data; new settings/rules and exact privileges pass'; fi
admin -c "DROP DATABASE $TAX_DB" >/dev/null; TAX_DB=""

# Customers/B2B must be additive over ALL preceding forward migrations. Match
# by logical suffix and filename order, never a reserved number or octal value.
customers=(migrations/[0-9][0-9][0-9]_customers_b2b.sql)
[[ ${#customers[@]} == 1 ]] || { echo 'FAIL: exactly one customers/B2B migration required' >&2; exit 1; }
fresh_before_customers() {
 admin -c "DROP DATABASE IF EXISTS $1" -c "CREATE DATABASE $1" 2>/dev/null
 local f
 for f in migrations/[0-9][0-9][0-9]_*.sql; do
  [[ $f < ${customers[0]} ]] || break
  apply_forward "$1" "$f"
 done
}
apply_customers() { { echo 'BEGIN;'; cat "${customers[0]}"; echo 'COMMIT;'; } | db "$1" 2>&1 >/dev/null; }
CUSTOMERS_DB=customers_upgrade_$$
fresh_before_customers "$CUSTOMERS_DB"
db "$CUSTOMERS_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('customer-upgrade') RETURNING id AS t \gset
INSERT INTO app.users(email,password_hash) VALUES('staff-upgrade@example.com','legacy-staff-hash') RETURNING id AS u \gset
INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(:'t',:'u','owner');
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','CUSTOMER-LEGACY') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 VALUES(:'t',:'p','CUSTOMER-LEGACY','default') RETURNING id AS k \gset
INSERT INTO app.price_lists(tenant_id,currency,starts_at) VALUES(:'t','USD','2026-01-01Z') RETURNING id AS l \gset
INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount)
 VALUES(:'t',:'l',:'k',1,199);
SQL
# Earlier cases created this cluster-wide role. Simulate a legacy deployment's
# unsafe attributes, memberships and independently granted column privileges;
# the forward migration must scrub every path before granting its narrow API.
db "$CUSTOMERS_DB" <<'SQL' >/dev/null
ALTER ROLE app_customer_auth LOGIN SUPERUSER BYPASSRLS CREATEDB CREATEROLE REPLICATION INHERIT;
GRANT app_auth TO app_customer_auth;
GRANT app_customer_auth TO app_runtime;
GRANT CREATE ON SCHEMA app TO app_customer_auth;
GRANT SELECT ON app.users TO app_customer_auth;
GRANT SELECT(password_hash) ON app.users TO app_customer_auth;
GRANT UPDATE(role) ON app.tenant_memberships TO app_customer_auth;
GRANT EXECUTE ON FUNCTION app.guard_release_children() TO app_customer_auth;
SQL
if ! out=$(apply_customers "$CUSTOMERS_DB"); then bad "populated customers/B2B upgrade: $out"
else
 result=$(db "$CUSTOMERS_DB" <<'SQL'
SELECT 'CUSTOMERS_ROLE_HYGIENE='||(
 NOT r.rolcanlogin AND NOT r.rolsuper AND NOT r.rolbypassrls AND NOT r.rolcreatedb
 AND NOT r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolinherit
 AND NOT EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid OR roleid=r.oid)
 AND has_schema_privilege(r.oid,'app','USAGE')
 AND NOT has_schema_privilege(r.oid,'app','CREATE')
 AND NOT has_table_privilege(r.oid,'app.users','SELECT')
 AND NOT has_column_privilege(r.oid,'app.users','password_hash','SELECT')
 AND NOT has_column_privilege(r.oid,'app.tenant_memberships','role','UPDATE')
 AND NOT has_function_privilege(r.oid,'app.guard_release_children()','EXECUTE'))::text
 FROM pg_roles r WHERE r.rolname='app_customer_auth';
SQL
 )
 if [[ $result == 'CUSTOMERS_ROLE_HYGIENE=true' ]]; then
  pass 'customers/B2B upgrade scrubs unsafe auth attributes, both membership directions, schema/table/column/function grants'
 else bad "customers/B2B role hygiene witness: $result"; fi
 result=$(db "$CUSTOMERS_DB" <<'SQL'
SELECT 'CUSTOMERS_UPGRADE='||
 (SELECT count(*) FROM app.users WHERE email='staff-upgrade@example.com' AND password_hash='legacy-staff-hash')||'|'||
 (SELECT count(*) FROM app.tenant_memberships m JOIN app.users u ON u.id=m.user_id WHERE u.email='staff-upgrade@example.com' AND m.role='owner' AND m.status='active')||'|'||
 (SELECT count(*) FROM app.price_entries e JOIN app.product_variants k ON (e.tenant_id,e.sku_id)=(k.tenant_id,k.id) WHERE k.sku='CUSTOMER-LEGACY' AND e.unit_amount=199)||'|'||
 (SELECT string_agg(code,',' ORDER BY code) FROM app.buyer_roles WHERE tenant_id=(SELECT id FROM app.tenants WHERE name='customer-upgrade'))||'|'||
 (SELECT count(*) FROM app.customers)||'|'||(SELECT count(*) FROM app.customer_credentials);
SQL
 )
 if [[ $result == 'CUSTOMERS_UPGRADE=1|1|1|admin,approver,purchaser|0|0' ]]; then
  pass 'customers/B2B upgrade preserves employee identity, membership and SKU pricing without synthesizing customers/credentials'
 else bad "customers/B2B populated upgrade witness: $result"; fi
 # Include any later module before evaluating the repository-wide role matrix.
 for f in migrations/[0-9][0-9][0-9]_*.sql; do
  [[ $f > ${customers[0]} ]] || continue
  apply_forward "$CUSTOMERS_DB" "$f"
 done
 if ! db "$CUSTOMERS_DB" -f tests/db_customers.sql >/dev/null; then bad 'customers/B2B SQL gate after populated upgrade'; fi
 if ! db "$CUSTOMERS_DB" -f tests/db_role_matrix.sql >/dev/null; then bad 'customers/B2B role matrix after populated upgrade'; fi
fi
admin -c "DROP DATABASE $CUSTOMERS_DB"; CUSTOMERS_DB=""

# Pricing intentionally had no company FK before B2B existed. An orphan legacy
# target requires operator reconciliation: the migration cannot silently clear
# its audience or install an unvalidated FK. Failure must roll back every DDL.
CUSTOMERS_DB=customers_orphan_upgrade_$$
fresh_before_customers "$CUSTOMERS_DB"
db "$CUSTOMERS_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('customer-orphan-upgrade') RETURNING id AS t \gset
INSERT INTO app.price_lists(tenant_id,currency,starts_at,audience,buyer_company_id)
 VALUES(:'t','USD','2026-01-01Z','buyer_company','00000000-0000-0000-0000-000000000005');
SQL
if out=$(apply_customers "$CUSTOMERS_DB"); then bad 'customers/B2B upgrade accepted an orphan legacy buyer-company price'
elif [[ $out != *CUSTOMERS_UPGRADE_BUYER_COMPANY_RECONCILIATION_REQUIRED* ]]; then bad "orphan customers/B2B upgrade failed for another reason: $out"
else
 result=$(db "$CUSTOMERS_DB" <<'SQL'
SELECT to_regclass('app.customers') IS NULL AND to_regclass('app.customer_credentials') IS NULL
 AND to_regclass('app.buyer_companies') IS NULL
 AND NOT EXISTS(SELECT FROM pg_constraint WHERE conrelid='app.price_lists'::regclass AND conname='price_lists_buyer_company_fk')
 AND (SELECT count(*) FROM app.price_lists WHERE audience='buyer_company' AND buyer_company_id='00000000-0000-0000-0000-000000000005')=1;
SQL
 )
 if [[ $result != t ]]; then bad "orphan customers/B2B refusal changed legacy data/schema: $result"
 else pass 'customers/B2B rejects orphan legacy buyer-price target atomically without changing audience'; fi
 # Explicit operator repair in the disposable fixture, then retry the SAME DDL.
 db "$CUSTOMERS_DB" -c "UPDATE app.price_lists SET audience='all',buyer_company_id=NULL" >/dev/null
 if ! out=$(apply_customers "$CUSTOMERS_DB"); then bad "customers/B2B retry after explicit reconciliation: $out"
 elif [[ $(db "$CUSTOMERS_DB" -c "SELECT convalidated FROM pg_constraint WHERE conrelid='app.price_lists'::regclass AND conname='price_lists_buyer_company_fk'") != t ]]; then bad 'customers/B2B retry left an unvalidated or missing buyer-price FK'
 else pass 'customers/B2B reconciliation retry installs the validated composite buyer-price FK'; fi
fi
admin -c "DROP DATABASE $CUSTOMERS_DB"; CUSTOMERS_DB=""

# Roles and pg_auth_members are cluster-wide. Grantor-aware cleanup must remove
# a non-superuser provisioner's outbound grant, keep a dedicated login provisioned
# for another database on the same cluster, and refuse (fail closed) when a
# third-party grantor's object privilege cannot be revoked by the owner.
drop_grantor_roles() {
 admin <<'SQL' >/dev/null 2>&1 || true
DO $d$ DECLARE m record; BEGIN
 FOR m IN SELECT r.rolname role_name,mem.rolname member_name,g.rolname grantor_name FROM pg_auth_members a
  JOIN pg_roles r ON r.oid=a.roleid JOIN pg_roles mem ON mem.oid=a.member JOIN pg_roles g ON g.oid=a.grantor
  WHERE g.rolname IN ('cust_provisioner','cust_thirdparty') LOOP
  EXECUTE format('REVOKE %I FROM %I GRANTED BY %I CASCADE',m.role_name,m.member_name,m.grantor_name);
 END LOOP; END $d$;
DROP ROLE IF EXISTS cust_provisioner; DROP ROLE IF EXISTS cust_thirdparty; DROP ROLE IF EXISTS cust_login_db1;
SQL
}
drop_grantor_roles
CUSTOMERS_DB=customers_grantor_upgrade_$$
fresh_before_customers "$CUSTOMERS_DB"
db "$CUSTOMERS_DB" <<'SQL' >/dev/null
DO $r$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='app_customer_auth') THEN
 CREATE ROLE app_customer_auth NOLOGIN; END IF; END $r$;
CREATE ROLE cust_provisioner NOLOGIN;
GRANT app_auth TO cust_provisioner WITH ADMIN OPTION;
SET ROLE cust_provisioner; GRANT app_auth TO app_customer_auth; RESET ROLE;
CREATE ROLE cust_login_db1 LOGIN;
GRANT app_customer_auth TO cust_login_db1;
SQL
if ! out=$(apply_customers "$CUSTOMERS_DB"); then bad "customers/B2B grantor upgrade: $out"
else
 result=$(db "$CUSTOMERS_DB" -c "SELECT pg_has_role('app_customer_auth','app_auth','SET')::text||'|'||pg_has_role('cust_login_db1','app_customer_auth','MEMBER')::text")
 [[ ${result%%|*} == false ]] && pass 'customers/B2B revokes a non-superuser-granted outbound membership by its grantor' \
  || bad "customers/B2B kept provisioner-granted app_auth membership: $result"
 [[ ${result##*|} == true ]] && pass 'customers/B2B keeps a dedicated customer-auth login provisioned for another database' \
  || bad "customers/B2B stripped a cluster-wide dedicated login: $result"
fi
admin -c "DROP DATABASE $CUSTOMERS_DB" >/dev/null; CUSTOMERS_DB=""
drop_grantor_roles
CUSTOMERS_DB=customers_thirdparty_upgrade_$$
fresh_before_customers "$CUSTOMERS_DB"
db "$CUSTOMERS_DB" <<'SQL' >/dev/null
DO $r$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='app_customer_auth') THEN
 CREATE ROLE app_customer_auth NOLOGIN; END IF; END $r$;
CREATE ROLE cust_thirdparty NOLOGIN;
GRANT USAGE ON SCHEMA app TO cust_thirdparty;
GRANT SELECT ON app.users TO cust_thirdparty WITH GRANT OPTION;
SET ROLE cust_thirdparty; GRANT SELECT ON app.users TO app_customer_auth; RESET ROLE;
SQL
if out=$(apply_customers "$CUSTOMERS_DB"); then bad 'customers/B2B accepted an unrevokable third-party grant to app_customer_auth'
elif [[ $out != *CUSTOMERS_AUTH_RESIDUAL_PRIVILEGE*users*cust_thirdparty* ]]; then bad "third-party grant refusal failed for another reason: $out"
else pass 'customers/B2B fails closed naming object and grantor of a residual third-party privilege'; fi
db "$CUSTOMERS_DB" -c "REVOKE ALL ON app.users FROM cust_thirdparty CASCADE; REVOKE ALL ON SCHEMA app FROM cust_thirdparty" >/dev/null 2>&1 || true
admin -c "DROP DATABASE $CUSTOMERS_DB" >/dev/null; CUSTOMERS_DB=""
drop_grantor_roles
# An inbound path through a non-app_ bridge group (app_auth -> legacy_bridge ->
# app_customer_auth) survives selective cleanup, so 016 must refuse it; once the
# bridge is removed the retry succeeds and a dedicated login keeps its membership.
drop_bridge_roles() { admin -c "DROP ROLE IF EXISTS legacy_bridge" -c "DROP ROLE IF EXISTS cust_login_ok" >/dev/null 2>&1 || true; }
drop_bridge_roles
CUSTOMERS_DB=customers_bridge_upgrade_$$
fresh_before_customers "$CUSTOMERS_DB"
db "$CUSTOMERS_DB" <<'SQL' >/dev/null
DO $r$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='app_customer_auth') THEN
 CREATE ROLE app_customer_auth NOLOGIN; END IF; END $r$;
CREATE ROLE legacy_bridge NOLOGIN;
GRANT app_customer_auth TO legacy_bridge;
GRANT legacy_bridge TO app_auth;
CREATE ROLE cust_login_ok LOGIN;
GRANT app_customer_auth TO cust_login_ok;
SQL
if out=$(apply_customers "$CUSTOMERS_DB"); then bad 'customers/B2B accepted an indirect app_auth path to app_customer_auth via a bridge group'
elif [[ $out != *CUSTOMERS_AUTH_FORBIDDEN_PATH*app_auth* ]]; then bad "bridge path refusal failed for another reason: $out"
else
 pass 'customers/B2B fails closed on an indirect internal-role path through a non-app_ bridge'
 admin -c "DROP ROLE legacy_bridge" >/dev/null
 if ! out=$(apply_customers "$CUSTOMERS_DB"); then bad "customers/B2B retry after bridge removal: $out"
 elif [[ $(db "$CUSTOMERS_DB" -c "SELECT pg_has_role('cust_login_ok','app_customer_auth','MEMBER')") != t ]]; then bad 'customers/B2B bridge refusal path stripped a dedicated login'
 else pass 'customers/B2B retry after bridge removal keeps the dedicated customer-auth login'; fi
fi
admin -c "DROP DATABASE $CUSTOMERS_DB" >/dev/null; CUSTOMERS_DB=""
drop_bridge_roles

# Migrator default privileges would pre-grant the new tables; 016 refuses them.
CUSTOMERS_DB=customers_defacl_upgrade_$$
fresh_before_customers "$CUSTOMERS_DB"
db "$CUSTOMERS_DB" <<'SQL' >/dev/null
DO $r$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='app_customer_auth') THEN
 CREATE ROLE app_customer_auth NOLOGIN; END IF; END $r$;
ALTER DEFAULT PRIVILEGES IN SCHEMA app GRANT ALL ON TABLES TO app_customer_auth;
SQL
if out=$(apply_customers "$CUSTOMERS_DB"); then bad 'customers/B2B accepted migrator default privileges granting app_customer_auth'
elif [[ $out != *CUSTOMERS_DEFAULT_PRIVILEGES*app_customer_auth* ]]; then bad "default-privilege refusal failed for another reason: $out"
else pass 'customers/B2B fails closed on default privileges that would pre-grant new tables'; fi
admin -c "DROP DATABASE $CUSTOMERS_DB" >/dev/null; CUSTOMERS_DB=""
# A preserved dedicated login must not keep ADMIN OPTION on the capability,
# otherwise it can delegate credential access to any role.
drop_admin_roles() { admin -c "DROP ROLE IF EXISTS cust_login_admin" -c "DROP ROLE IF EXISTS cust_victim" >/dev/null 2>&1 || true; }
drop_admin_roles
CUSTOMERS_DB=customers_admin_upgrade_$$
fresh_before_customers "$CUSTOMERS_DB"
db "$CUSTOMERS_DB" <<'SQL' >/dev/null
DO $r$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='app_customer_auth') THEN
 CREATE ROLE app_customer_auth NOLOGIN; END IF; END $r$;
CREATE ROLE cust_login_admin LOGIN;
CREATE ROLE cust_victim NOLOGIN;
GRANT app_customer_auth TO cust_login_admin WITH ADMIN OPTION;
SQL
if ! out=$(apply_customers "$CUSTOMERS_DB"); then bad "customers/B2B admin-option upgrade: $out"
else
 result=$(db "$CUSTOMERS_DB" -c "SELECT pg_has_role('cust_login_admin','app_customer_auth','MEMBER')::text||'|'||bool_or(admin_option)::text FROM pg_auth_members WHERE roleid='app_customer_auth'::regrole AND member='cust_login_admin'::regrole")
 if [[ $result != 'true|false' ]]; then bad "customers/B2B preserved login membership/admin option: $result"
 elif db "$CUSTOMERS_DB" -c "SET SESSION AUTHORIZATION cust_login_admin; GRANT app_customer_auth TO cust_victim" >/dev/null 2>&1; then bad 'customers/B2B preserved login can still delegate app_customer_auth'
 else pass 'customers/B2B keeps a dedicated login but strips its admin option (no delegation)'; fi
fi
admin -c "DROP DATABASE $CUSTOMERS_DB" >/dev/null; CUSTOMERS_DB=""
drop_admin_roles

# A tenant inserted concurrently (uncommitted when 016 starts) must end up with
# exactly the three buyer roles: backfill and trigger leave no gap between them.
CUSTOMERS_DB=customers_tenant_race_$$
fresh_before_customers "$CUSTOMERS_DB"
db "$CUSTOMERS_DB" <<'SQL' >/dev/null &
BEGIN;
INSERT INTO app.tenants(name) VALUES('race-tenant');
SELECT pg_sleep(0.5);
DO $w$ BEGIN FOR i IN 1..300 LOOP PERFORM pg_stat_clear_snapshot();
 EXIT WHEN EXISTS(SELECT FROM pg_stat_activity WHERE datname=current_database()
  AND pid<>pg_backend_pid() AND wait_event_type='Lock');
 PERFORM pg_sleep(0.1); END LOOP; END $w$;
COMMIT;
SQL
race_pid=$!
until [[ $(db "$CUSTOMERS_DB" -c "SELECT count(*) FROM pg_locks WHERE relation='app.tenants'::regclass AND mode='RowExclusiveLock' AND granted") != 0 ]]; do sleep 0.1; done
out=$(apply_customers "$CUSTOMERS_DB") || bad "customers/B2B upgrade with concurrent tenant insert: $out"
wait "$race_pid" || bad 'customers/B2B concurrent tenant insert session failed'
result=$(db "$CUSTOMERS_DB" -c "SELECT string_agg(code,',' ORDER BY code) FROM app.buyer_roles r JOIN app.tenants t ON t.id=r.tenant_id WHERE t.name='race-tenant'")
[[ $result == admin,approver,purchaser ]] && pass 'customers/B2B concurrent tenant insert gets exactly the three buyer roles' \
 || bad "customers/B2B concurrent tenant buyer roles: $result"
admin -c "DROP DATABASE $CUSTOMERS_DB" >/dev/null; CUSTOMERS_DB=""
# Ledger upgrade is additive: a tenant created before the logical migration
# remains explicitly unconfigured. Currency is an operator choice, never a
# guessed USD backfill. All predecessors/successors use filename order.
ledger=(migrations/[0-9][0-9][0-9]_ledger.sql)
[[ ${#ledger[@]} == 1 ]] || { echo 'FAIL: exactly one ledger migration required' >&2; exit 1; }
LEDGER_DB=ledger_upgrade_$$
admin -c "CREATE DATABASE $LEDGER_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${ledger[0]} ]] || break
 apply_forward "$LEDGER_DB" "$f"
done
db "$LEDGER_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('ledger-existing-tenant') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','ledger-existing-product');
SQL
{ echo 'BEGIN;'; cat "${ledger[0]}"; echo 'COMMIT;'; } | db "$LEDGER_DB" >/dev/null
result=$(db "$LEDGER_DB" <<'SQL'
SELECT 'LEDGER_UNCONFIGURED='||count(*) FROM app.tenants
 WHERE name='ledger-existing-tenant' AND settlement_currency IS NULL;
UPDATE app.tenants SET settlement_currency='USD' WHERE name='ledger-existing-tenant' RETURNING id AS t \gset
BEGIN;
SELECT set_config('app.tenant_id',:'t',true);
SET LOCAL ROLE app_runtime;
INSERT INTO app.ledger_accounts(tenant_id,code,account_type,currency)
 VALUES(:'t','cash','asset','USD') RETURNING id AS a \gset
INSERT INTO app.ledger_accounts(tenant_id,code,account_type,currency)
 VALUES(:'t','revenue','revenue','USD') RETURNING id AS b \gset
SELECT app.post_ledger(:'t','USD','upgrade','legacy','capture',ARRAY[:'a',:'b']::uuid[],ARRAY['debit','credit'],ARRAY[125,125]::bigint[]);
COMMIT;
BEGIN;
SELECT set_config('app.tenant_id',:'t',true);
SET LOCAL ROLE app_runtime;
SELECT 'LEDGER_UPGRADE='||(SELECT count(*) FROM app.products WHERE product_code='ledger-existing-product')||'|'||
 (SELECT count(*) FROM app.ledger_transactions WHERE status='posted' AND posted_at IS NOT NULL)||'|'||
 (SELECT count(*) FROM app.ledger_entries)||'|'||
 (SELECT sum(CASE side WHEN 'debit' THEN amount::numeric ELSE -amount::numeric END) FROM app.ledger_entries);
COMMIT;
SQL
)
[[ $result == *'LEDGER_UNCONFIGURED=1'* && $result == *'LEDGER_UPGRADE=1|1|2|0'* ]] || bad "ledger populated upgrade witness: $result"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${ledger[0]} ]] || continue
 apply_forward "$LEDGER_DB" "$f"
done
if ! db "$LEDGER_DB" < tests/db_ledger.sql >/dev/null || ! db "$LEDGER_DB" < tests/db_role_matrix.sql >/dev/null; then
 bad 'ledger constraints/role matrix after populated upgrade'
else pass 'ledger upgrade preserves existing catalog/unconfigured tenant; explicit currency runtime posting and exact privileges pass'; fi
admin -c "DROP DATABASE $LEDGER_DB" >/dev/null; LEDGER_DB=""

# Catalog API: apply every earlier forward migration by file order, then prove
# legacy product/SKU/media metadata survives the additive columns and grants.
catalog_api=(migrations/[0-9][0-9][0-9]_catalog_api.sql)
[[ ${#catalog_api[@]} == 1 ]] || { echo 'FAIL: exactly one catalog API migration required' >&2; exit 1; }
CATALOG_DB=catalog_api_upgrade_$$
admin -c "CREATE DATABASE $CATALOG_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${catalog_api[0]} ]] || break
 apply_forward "$CATALOG_DB" "$f"
done
db "$CATALOG_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('catalog-existing') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','catalog-existing') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'p','catalog-existing','');
INSERT INTO app.media_uploads(tenant_id,original_filename,expected_size,expires_at) VALUES(:'t','old.png',0,now()+interval '1 hour');
SQL
{ echo 'BEGIN;'; cat "${catalog_api[0]}"; echo 'COMMIT;'; } | db "$CATALOG_DB" >/dev/null
result=$(db "$CATALOG_DB" <<'SQL'
SELECT (SELECT version FROM app.products WHERE product_code='catalog-existing')||'|'||
 (SELECT version FROM app.product_variants WHERE sku='catalog-existing')||'|'||
 (SELECT lifecycle_status FROM app.product_variants WHERE sku='catalog-existing')||'|'||
 (SELECT count(*) FROM app.media_uploads WHERE original_filename='old.png' AND media_id IS NULL);
SQL
)
[[ $result == *'1|1|draft|1'* ]] || bad "catalog populated upgrade witness: $result"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${catalog_api[0]} ]] || continue
 apply_forward "$CATALOG_DB" "$f"
done
if ! db "$CATALOG_DB" < tests/db_catalog_api.sql >/dev/null || ! db "$CATALOG_DB" < tests/db_role_matrix.sql >/dev/null; then
 bad 'catalog constraints/role matrix after populated upgrade'
else pass 'catalog additive upgrade preserves versions, lifecycle and old uploads'; fi
admin -c "DROP DATABASE $CATALOG_DB" >/dev/null; CATALOG_DB=""

# C04b: queue authorization over existing tenant + River job, suffix-discovered.
queue_files=(migrations/[0-9][0-9][0-9]_queue_authorization.sql)
[[ ${#queue_files[@]} -eq 1 && -f ${queue_files[0]} ]] || { echo 'FAIL: exactly one queue authorization migration required'; exit 1; }
QUEUE_DB=upgrade_queue_$$; DB=$QUEUE_DB
admin -c "DROP DATABASE IF EXISTS $DB" -c "CREATE DATABASE $DB" 2>/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${queue_files[0]} ]] || break
 apply_forward "$DB" "$f"
done
river_db "$DB"
river_db "$DB"  # repeated upstream migration is also safe before app authorization
legacy=$(db "$DB" <<'SQL'
INSERT INTO app.tenants(name) VALUES('queue-upgrade') RETURNING id AS t \gset
INSERT INTO queue.river_job(args,kind,queue)
 VALUES(jsonb_build_object('tenant_id',:'t'),'queue_legacy','default') RETURNING id;
SQL
)
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${queue_files[0]} ]] && continue
 apply_forward "$DB" "$f"
done
river_db "$DB" --verify
[[ $(db "$DB" -c "SELECT count(*) FROM queue.river_job WHERE kind='queue_legacy' AND id=$legacy AND args->>'tenant_id'=(SELECT id::text FROM app.tenants WHERE name='queue-upgrade')") == 1 ]] || { bad 'queue upgrade changed existing job/tenant identity'; }
db "$DB" -f tests/db_queue.sql >/dev/null
db "$DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $DB"; QUEUE_DB=""
pass 'queue upgrade preserves job identity; isolated ACL/RLS + both upstream history passes'

# INT25: populate old delivery epochs before the logical limits migration.
limits_files=(migrations/[0-9][0-9][0-9]_reliability_limits.sql)
[[ ${#limits_files[@]} == 1 ]] || { bad 'exactly one reliability-limits migration required'; exit 1; }
LIMITS_DB=upgrade_limits_$$; DB=$LIMITS_DB
admin -c "CREATE DATABASE $DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${limits_files[0]} ]] || break
 apply_forward "$DB" "$f"
done
db "$DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('limits-existing') RETURNING id AS t \gset
INSERT INTO app.outbox_events(tenant_id,id,event_type,payload,delivery_target,dedup_key,status,attempts,lease_owner,lease_until,delivered_at)
VALUES
(:'t','00000000-0000-4000-8000-000000000001','legacy','{}','test','failed','failed',10,NULL,NULL,NULL),
(:'t','00000000-0000-4000-8000-000000000002','legacy','{}','test','crashed','processing',10,'11111111-1111-4111-8111-111111111111',clock_timestamp()-interval '1 second',NULL),
(:'t','00000000-0000-4000-8000-000000000003','legacy','{}','test','live','processing',10,'11111111-1111-4111-8111-111111111111',clock_timestamp()+interval '1 hour',NULL),
(:'t','00000000-0000-4000-8000-000000000004','legacy','{}','test','delivered','delivered',2,NULL,NULL,clock_timestamp());
SQL
apply_forward "$DB" "${limits_files[0]}"
result=$(db "$DB" <<'SQL'
SELECT set_config('app.tenant_id',(SELECT id::text FROM app.tenants WHERE name='limits-existing'),false);
SET ROLE app_outbox_worker;
SELECT count(*) FROM app.claim_outbox(current_setting('app.tenant_id')::uuid,'22222222-2222-4222-8222-222222222222',10,60);
SELECT app.complete_outbox(current_setting('app.tenant_id')::uuid,'00000000-0000-4000-8000-000000000003','11111111-1111-4111-8111-111111111111',10);
RESET ROLE;
SELECT 'LIMITS_UPGRADE='||count(*)||'|'||count(*) FILTER(WHERE status='dead_letter')||'|'||count(*) FILTER(WHERE status='delivered')||'|'||max(attempts) FROM app.outbox_events;
SQL
)
[[ $result == *'LIMITS_UPGRADE=4|2|2|10'* ]] || bad "limits populated upgrade: $result"
# Complete the current app schema before the exact whole-schema role matrix.
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${limits_files[0]} ]] || continue
 apply_forward "$DB" "$f"
done
river_db "$DB" --verify
db "$DB" -f tests/db_reliability_limits.sql >/dev/null
db "$DB" -f tests/db_ops_metrics.sql >/dev/null
db "$DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $DB"; LIMITS_DB=""
pass 'reliability limits upgrade preserves delivery identity, retires exhausted work, accepts live final ACK'

[[ $fail -eq 0 ]] || exit 1
# C-15a: install orders over all preceding forward migrations and preserve a
# preexisting tenant/SKU/owned active reservation plus its stock movement history.
orders_files=(migrations/[0-9][0-9][0-9]_orders.sql)
[[ ${#orders_files[@]} -eq 1 && -f ${orders_files[0]} ]] || { echo 'FAIL: exactly one orders migration required'; exit 1; }
ORDERS_DB=upgrade_orders_$$; DB=$ORDERS_DB
admin -c "DROP DATABASE IF EXISTS $DB" -c "CREATE DATABASE $DB" 2>/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${orders_files[0]} ]] || break
 apply_forward "$DB" "$f"
done
db "$DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('orders-upgrade') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','orders-legacy') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 VALUES(:'t',:'p','orders-legacy','default') RETURNING id AS sku \gset
SELECT set_config('app.tenant_id',:'t',false);
SET ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',3,'legacy receipt',uuidv7(),'inbound');
RESET ROLE;
SET ROLE app_runtime;
SELECT uuidv7() AS oid \gset
SELECT app.inventory_reserve(:'sku',1,'order',:'oid','b596ffa1-ff67-55f1-ae2b-fddf0f219427',900);
SQL
{ echo 'BEGIN;'; cat "${orders_files[0]}"; echo 'COMMIT;'; } | db "$DB" >/dev/null
result=$(db "$DB" <<'SQL'
SELECT id AS t FROM app.tenants WHERE name='orders-upgrade' \gset
SELECT id AS sku FROM app.product_variants WHERE sku='orders-legacy' \gset
SELECT id AS rid,owner_id AS oid FROM app.inventory_reservations WHERE tenant_id=:'t' \gset
SELECT set_config('app.tenant_id',:'t',false);
SET ROLE app_runtime;
SELECT app.orders_create(:'oid','c814a75e-b677-584b-b658-9cc492943b00',sha256('legacy'::bytea),NULL,
 '{"email":"legacy@example.invalid"}','USD',20,'{"currency":"USD","amount_minor":20,"service":"fixture"}',
 jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',:'sku','sku','orders-legacy','quantity',1,
 'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',10,'line_total_minor',110,
 'reservation_id',:'rid','tax_snapshot',jsonb_build_object('category','standard','net_minor',100,'tax_minor',10,'gross_minor',110,'currency','USD'))),'[]');
SELECT app.orders_transition(:'oid','paid','legacy-pay',0);
SELECT 'ORDERS_UPGRADE='||o.order_number||'|'||o.state||'|'||o.total_minor||'|'||s.on_hand||'|'||s.reserved||'|'||
 (SELECT count(*) FROM app.inventory_movements WHERE tenant_id=:'t')||'|'||
 (SELECT count(*) FROM app.outbox_events WHERE tenant_id=:'t' AND payload->>'order_id'=:'oid')||'|'||
 has_function_privilege('app_runtime','app.inventory_commit(uuid)','EXECUTE')
 FROM app.orders o JOIN app.stock_levels s USING(tenant_id) WHERE o.tenant_id=:'t';
SQL
)
if [[ $result == *'ORDERS_UPGRADE=1|paid|130|2|0|3|2|false'* ]]; then
 pass 'orders upgrade preserves legacy reservation/stock and fences runtime terminals'
else bad "orders upgrade witness: $result"; fi
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${orders_files[0]} ]] || continue
 apply_forward "$DB" "$f"
done
river_db "$DB" --verify
db "$DB" -f tests/db_orders.sql >/dev/null
db "$DB" -f tests/db_orders_guards.sql >/dev/null
db "$DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $DB"; ORDERS_DB=""
# Cart additive upgrade: all predecessors, populated customers/catalog, then
# the logical *_cart suffix. Number reservations can be reconciled without drift.
cart=(migrations/[0-9][0-9][0-9]_cart.sql)
[[ ${#cart[@]} == 1 && -f ${cart[0]} ]] || { echo 'FAIL: exactly one cart migration required' >&2; exit 1; }
CART_DB=cart_upgrade_$$
admin -c "CREATE DATABASE $CART_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${cart[0]} ]] || break
 apply_forward "$CART_DB" "$f"
done
db "$CART_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('cart-existing-tenant') RETURNING id AS t \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'t','cart-upgrade@example.test');
INSERT INTO app.products(tenant_id,product_code,lifecycle_status) VALUES(:'t','cart-existing-product','ready') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'p','cart-existing-sku','default');
SQL
{ echo 'BEGIN;'; cat "${cart[0]}"; echo 'COMMIT;'; } | db "$CART_DB" >/dev/null
result=$(db "$CART_DB" <<'SQL'
SELECT t.id AS t,c.id AS customer,v.id AS sku FROM app.tenants t
JOIN app.customers c ON c.tenant_id=t.id JOIN app.product_variants v ON v.tenant_id=t.id
WHERE t.name='cart-existing-tenant' \gset
BEGIN;
SELECT set_config('app.tenant_id',:'t',true),set_config('app.cart_customer_id',:'customer',true),set_config('app.cart_token_hash','',true);
SET LOCAL ROLE app_runtime;
INSERT INTO app.carts(tenant_id,customer_id,currency) VALUES(:'t',:'customer','USD') RETURNING id AS cart \gset
INSERT INTO app.cart_lines VALUES(:'t',:'cart',:'sku',2);
UPDATE app.carts SET version=version+1 WHERE id=:'cart' AND version=1;
SELECT 'CART_UPGRADE='||(SELECT version FROM app.carts WHERE id=:'cart')||'|'||
 (SELECT quantity FROM app.cart_lines WHERE cart_id=:'cart')||'|'||
 (SELECT count(*) FROM app.product_variants WHERE sku='cart-existing-sku')||'|'||
 (SELECT count(*) FROM app.customers WHERE email='cart-upgrade@example.test');
COMMIT;
SQL
)
[[ $result == *'CART_UPGRADE=2|2|1|1'* ]] || bad "cart populated upgrade witness: $result"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${cart[0]} ]] || continue
 apply_forward "$CART_DB" "$f"
done
if ! db "$CART_DB" < tests/db_cart.sql >/dev/null || ! db "$CART_DB" < tests/db_role_matrix.sql >/dev/null; then
 bad 'cart owner/constraints/role matrix after populated upgrade'
else pass 'cart upgrade preserves prior customers/catalog; owner/CAS and exact privilege gate pass'; fi
admin -c "DROP DATABASE $CART_DB" >/dev/null; CART_DB=""

# Public content capability is added only after all logical predecessors; the
# same committed release/secret witnesses survive the forward role migration.
storefront=(migrations/[0-9][0-9][0-9]_storefront_read.sql)
[[ ${#storefront[@]} == 1 ]] || { echo 'FAIL: exactly one storefront read migration required' >&2; exit 1; }
STOREFRONT_DB=storefront_upgrade_$$
admin -c "CREATE DATABASE $STOREFRONT_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${storefront[0]} ]] || break
 apply_forward "$STOREFRONT_DB" "$f"
done
db "$STOREFRONT_DB" < tests/fixtures/storefront_seed.sql >/dev/null
before=$(db "$STOREFRONT_DB" -c "SELECT has_column_privilege('app_domain_resolver','app.page_revisions','document','SELECT')")
[[ $before == f ]] || bad 'storefront predecessors unexpectedly grant page content'
if red=$(db "$STOREFRONT_DB" < tests/db_storefront.sql 2>&1); then
 bad 'storefront new SQL invariants unexpectedly pass before capability migration'
elif [[ $red == *'permission denied for table site_releases'* ]]; then
 pass 'storefront pre-migration negative: published content capability is absent (42501)'
else bad "storefront pre-migration failed for an unexpected reason: $red"; fi
apply_forward "$STOREFRONT_DB" "${storefront[0]}"
result=$(db "$STOREFRONT_DB" <<'SQL'
BEGIN READ ONLY;
SET LOCAL ROLE app_domain_resolver;
SELECT set_config('app.public_hostname','localhost',true);
SELECT 'STOREFRONT_UPGRADE='||(SELECT count(*) FROM app.site_releases)||'|'||
 (SELECT count(*) FROM app.page_revisions)||'|'||
 (SELECT count(*) FROM app.page_revisions WHERE document::text LIKE '%DRAFT%');
COMMIT;
SQL
)
[[ $result == *'STOREFRONT_UPGRADE=1|2|0'* ]] || bad "storefront populated upgrade witness: $result"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${storefront[0]} ]] || continue
 apply_forward "$STOREFRONT_DB" "$f"
done
river_db "$STOREFRONT_DB" --verify
if ! db "$STOREFRONT_DB" < tests/db_storefront.sql >/dev/null || ! db "$STOREFRONT_DB" < tests/db_role_matrix.sql >/dev/null; then
 bad 'storefront RLS/role matrix after populated upgrade'
else pass 'storefront upgrade exposes current sealed pinned pages; draft witnesses stay hidden'; fi
admin -c "DROP DATABASE $STOREFRONT_DB" >/dev/null; STOREFRONT_DB=""

# C23a: add only the metrics capability over populated pending and DLQ-only
# tenants; preserve event identity/content and every prior forward migration.
ops_metrics=(migrations/[0-9][0-9][0-9]_ops_metrics.sql)
[[ ${#ops_metrics[@]} == 1 ]] || { bad 'exactly one ops-metrics migration required'; exit 1; }
OPS_METRICS_DB=upgrade_ops_metrics_$$; DB=$OPS_METRICS_DB
admin -c "CREATE DATABASE $DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${ops_metrics[0]} ]] || break
 apply_forward "$DB" "$f"
done
db "$DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('ops-existing-pending') RETURNING id AS ta \gset
INSERT INTO app.tenants(name) VALUES('ops-existing-DLQ-only') RETURNING id AS tb \gset
INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key,status,created_at)
 VALUES(:'ta','ops.legacy','{"witness":"pending"}','test','legacy-pending','pending',clock_timestamp()-interval '1 minute'),
       (:'tb','ops.legacy','{"witness":"dead"}','test','legacy-dead','dead_letter',clock_timestamp()-interval '1 hour');
SQL
apply_forward "$DB" "${ops_metrics[0]}"
result=$(db "$DB" <<'SQL'
SELECT 'OPS_PRESERVED='||count(*)||'|'||count(*) FILTER(WHERE status='pending' AND payload->>'witness'='pending')||'|'||
 count(*) FILTER(WHERE status='dead_letter' AND payload->>'witness'='dead') FROM app.outbox_events;
SET ROLE app_outbox_worker;
SELECT 'OPS_METRICS='||pending||'|'||dead_letters||'|'||(oldest_pending_age_seconds>=60) FROM app.outbox_metrics();
RESET ROLE;
SQL
)
[[ $result == *'OPS_PRESERVED=2|1|1'* && $result == *'OPS_METRICS=1|1|true'* ]] || bad "ops-metrics populated upgrade: $result"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${ops_metrics[0]} ]] || continue
 apply_forward "$DB" "$f"
done
db "$DB" -f tests/db_ops_metrics.sql >/dev/null
db "$DB" -f tests/db_role_matrix.sql >/dev/null
river_db "$DB" --verify
admin -c "DROP DATABASE $DB"; OPS_METRICS_DB=""
pass 'ops metrics preserves pending/DLQ-only tenants and exposes only aggregate worker gauges'
# Notifications additive upgrade: every actual predecessor, populated original
# order event retained verbatim; historical source events are not silently sent.
notifications=(migrations/[0-9][0-9][0-9]_notifications.sql)
[[ ${#notifications[@]} == 1 ]] || { echo 'FAIL: one notifications migration required' >&2; exit 1; }
NOTIFICATIONS_DB=notifications_upgrade_$$
admin -c "CREATE DATABASE $NOTIFICATIONS_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${notifications[0]} ]] || break
 apply_forward "$NOTIFICATIONS_DB" "$f"
done
db "$NOTIFICATIONS_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('notification-upgrade') RETURNING id AS t \gset
SELECT set_config('app.tenant_id',:'t',false);
SET ROLE app_orders_executor;
INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
VALUES(:'t',uuidv7(),1,'6c045ad4-5d06-55f1-a725-cf7dadb256a6',sha256('upgrade'::bytea),'{"email":"upgrade@example.test"}','USD',100,0,0,'{}',100) RETURNING id AS oid \gset
RESET ROLE;
INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
VALUES(:'t','order.paid',jsonb_build_object('order_id',:'oid'),'orders.events','upgrade-source');
SQL
apply_forward "$NOTIFICATIONS_DB" "${notifications[0]}"
result=$(db "$NOTIFICATIONS_DB" <<'SQL'
SELECT t.id AS t,o.id AS oid FROM app.tenants t JOIN app.orders o ON o.tenant_id=t.id WHERE t.name='notification-upgrade' \gset
SELECT set_config('app.tenant_id',:'t',false);
SELECT 'NOTIFICATIONS_UPGRADE='||(SELECT count(*) FROM app.outbox_events WHERE tenant_id=:'t' AND delivery_target='orders.events' AND status='pending' AND attempts=0 AND dedup_key='upgrade-source' AND payload=jsonb_build_object('order_id',:'oid'))||'|'||(SELECT count(*) FROM app.notification_deliveries WHERE tenant_id=:'t');
INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES(:'t','order.fulfilled',jsonb_build_object('order_id',:'oid'),'orders.events','upgrade-new');
SELECT 'NOTIFICATIONS_NEW='||(SELECT count(*) FROM app.notification_deliveries WHERE tenant_id=:'t' AND recipient='upgrade@example.test' AND template_key='order.shipped')||'|'||(SELECT count(*) FROM app.outbox_events WHERE tenant_id=:'t' AND delivery_target='notifications.email');
SQL
)
[[ $result == *'NOTIFICATIONS_UPGRADE=1|0'* && $result == *'NOTIFICATIONS_NEW=1|1'* ]] || bad "notification populated upgrade witness: $result"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${notifications[0]} ]] || continue
 apply_forward "$NOTIFICATIONS_DB" "$f"
done
db "$NOTIFICATIONS_DB" -f tests/db_notifications.sql >/dev/null
db "$NOTIFICATIONS_DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $NOTIFICATIONS_DB" >/dev/null; NOTIFICATIONS_DB=""
pass 'notifications upgrade preserves original order outbox and fans out only newly inserted events'


# C-22a data rights upgrade BEGIN: select by logical suffix and apply every
# predecessor before seeding. Existing customer/address/order rows are hashed
# before/after the forward migration; privacy and whole-schema authority gates
# run after every successor. The throwaway DB joins the normal EXIT cleanup.
data_rights_files=(migrations/[0-9][0-9][0-9]_data_rights.sql)
[[ ${#data_rights_files[@]} == 1 && -f ${data_rights_files[0]} ]] || { echo 'FAIL: exactly one data rights migration required' >&2; exit 1; }
DATA_RIGHTS_DB=data_rights_upgrade_$$; DB=$DATA_RIGHTS_DB
admin -c "CREATE DATABASE $DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${data_rights_files[0]} ]] || break
 apply_forward "$DB" "$f"
done
db "$DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('data-rights-existing') RETURNING id AS t \gset
INSERT INTO app.customers(tenant_id,email,display_name) VALUES(:'t','legacy-privacy@example.invalid','Synthetic legacy') RETURNING id AS customer \gset
-- Pre-existing addresses in the newly reserved erasure namespace stay valid
-- legacy rows; the reservation must not fail the upgrade or rewrite them.
INSERT INTO app.customers(tenant_id,email,display_name) VALUES(:'t','legacy-domain@erased.invalid','Synthetic legacy domain');
INSERT INTO app.customer_addresses(tenant_id,customer_id,recipient,address_line1,city,country_code)
 VALUES(:'t',:'customer','Synthetic legacy','Synthetic address','Synthetic city','US');
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','privacy-legacy') RETURNING id AS product \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'product','privacy-legacy-sku','default') RETURNING id AS sku \gset
SELECT set_config('app.tenant_id',:'t',false);
SET ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',2,'synthetic privacy upgrade',uuidv7(),'inbound');
RESET ROLE;
SET ROLE app_runtime;
SELECT uuidv7() AS oid \gset
SELECT (app.inventory_reserve(:'sku',1,'order',:'oid','542ebc5b-2736-50dd-91f8-4780722cc26b',900)).id AS rid \gset
SELECT app.orders_create(:'oid','41f67f2c-4129-55be-8b58-0b53f0964ced',sha256('legacy'::bytea),:'customer',NULL,'USD',20,
 '{"currency":"USD","amount_minor":20,"recipient":"Synthetic legacy"}',
 jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',:'sku','sku','privacy-legacy-sku','quantity',1,
 'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',10,'line_total_minor',110,
 'reservation_id',:'rid','tax_snapshot',jsonb_build_object('category','standard','net_minor',100,'tax_minor',10,'gross_minor',110,'currency','USD'))),'[]');
SQL
privacy_existing_witness() {
 db "$1" <<'SQL'
SELECT id AS t FROM app.tenants WHERE name='data-rights-existing' \gset
SELECT encode(sha256(jsonb_build_object(
 'customers',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM app.customers c WHERE tenant_id=:'t'),
 'addresses',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM app.customer_addresses a WHERE tenant_id=:'t'),
 'orders',(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM app.orders o WHERE tenant_id=:'t'),
 'lines',(SELECT jsonb_agg(to_jsonb(l) ORDER BY id) FROM app.order_lines l WHERE tenant_id=:'t'))::text::bytea),'hex')||'|'||
 (SELECT count(*) FROM app.customers WHERE tenant_id=:'t')||'|'||
 (SELECT count(*) FROM app.customer_addresses WHERE tenant_id=:'t')||'|'||
 (SELECT count(*) FROM app.orders WHERE tenant_id=:'t')||'|'||
 (SELECT count(*) FROM app.order_lines WHERE tenant_id=:'t');
SQL
}
privacy_before=$(privacy_existing_witness "$DB")
[[ $privacy_before == *'|2|1|1|1' ]] || bad "privacy predecessor fixture is incomplete: $privacy_before"
apply_forward "$DB" "${data_rights_files[0]}"
privacy_after=$(privacy_existing_witness "$DB")
[[ $privacy_before == "$privacy_after" ]] || bad 'data rights migration changed populated customer/address/order/line rows'
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${data_rights_files[0]} ]] || continue
 apply_forward "$DB" "$f"
done
river_db "$DB" --verify
if ! db "$DB" < tests/db_data_rights.sql >/dev/null || ! db "$DB" < tests/db_role_matrix.sql >/dev/null; then
 bad 'data rights controlled functions or exact authority after populated upgrade'
else
 pass 'data rights upgrade preserves customer/address/order/line bytes; privacy and exact authority pass'
fi
admin -c "DROP DATABASE $DB" >/dev/null; DATA_RIGHTS_DB=""
# C-22a data rights upgrade END

# C-02a: discover the identity upgrade by suffix. Seed only after ALL filename-
# ordered predecessors; retain a full-row snapshot so ids, hashes and state are
# independently compared, then apply every later module before the exact matrix.
identity_files=(migrations/[0-9][0-9][0-9]_identity_auth.sql)
[[ ${#identity_files[@]} -eq 1 && -f ${identity_files[0]} ]] || { echo 'FAIL: exactly one identity auth migration required'; exit 1; }
IDENTITY_DB=upgrade_identity_$$
DB=$IDENTITY_DB
admin -c "DROP DATABASE IF EXISTS $DB" -c "CREATE DATABASE $DB" 2>/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${identity_files[0]} ]] || break
 apply_forward "$DB" "$f"
done
db "$DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name,status) VALUES('identity-upgrade','suspended') RETURNING id AS t \gset
INSERT INTO app.users(email,password_hash,status)
 VALUES('identity-upgrade@example.test','legacy-preserved-password-hash','disabled') RETURNING id AS u \gset
INSERT INTO app.tenant_memberships(tenant_id,user_id,role,status)
 VALUES(:'t',:'u','viewer','suspended');
INSERT INTO app.api_keys(tenant_id,issued_by,key_prefix,key_hash,scopes,expires_at,revoked_at)
 VALUES(:'t',:'u','legacy-upgrade',sha256('identity-upgrade-key'::bytea),ARRAY['catalog:read'],
        '2031-01-01T00:00:00Z','2030-01-01T00:00:00Z');
CREATE TABLE public.identity_upgrade_expected(entity text PRIMARY KEY,payload jsonb NOT NULL);
INSERT INTO public.identity_upgrade_expected
 SELECT 'tenant',to_jsonb(t) FROM app.tenants t WHERE t.id=:'t'
 UNION ALL SELECT 'user',to_jsonb(u) FROM app.users u WHERE u.id=:'u'
 UNION ALL SELECT 'membership',to_jsonb(m) FROM app.tenant_memberships m WHERE m.tenant_id=:'t' AND m.user_id=:'u'
 UNION ALL SELECT 'api_key',to_jsonb(k) FROM app.api_keys k WHERE k.tenant_id=:'t' AND k.issued_by=:'u';
SQL
apply_forward "$DB" "${identity_files[0]}"
result=$(db "$DB" <<'SQL'
WITH actual(entity,payload) AS (
 SELECT 'tenant',to_jsonb(t) FROM app.tenants t WHERE t.name='identity-upgrade'
 UNION ALL SELECT 'user',to_jsonb(u) FROM app.users u WHERE u.email_normalized='identity-upgrade@example.test'
 UNION ALL SELECT 'membership',to_jsonb(m) FROM app.tenant_memberships m JOIN app.users u ON u.id=m.user_id WHERE u.email_normalized='identity-upgrade@example.test'
 UNION ALL SELECT 'api_key',to_jsonb(k) FROM app.api_keys k WHERE k.key_prefix='legacy-upgrade'
)
SELECT 'IDENTITY_UPGRADE='||(SELECT count(*) FROM public.identity_upgrade_expected)||'|'||
 (SELECT count(*) FROM actual)||'|'||
 (SELECT count(*) FROM actual a JOIN public.identity_upgrade_expected e USING(entity,payload));
SQL
)
if [[ $result == 'IDENTITY_UPGRADE=4|4|4' ]]; then pass 'identity upgrade preserves all legacy user/tenant/viewer/API-key ids, hashes and state'
else bad "identity upgrade witness: $result"; fi
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${identity_files[0]} ]] || continue
 apply_forward "$DB" "$f"
done

# Execute-only bootstrap after a populated identity upgrade, with no prior owner.
# Every call uses a real SET ROLE transaction, including the isolation rejection.
# The deliberately duplicate email must leave no new tenant/user/owner behind.
if out=$(db "$DB" -v VERBOSITY=verbose 2>&1 <<'SQL'
BEGIN ISOLATION LEVEL READ COMMITTED;
SET LOCAL ROLE app_admin_bootstrap;
SELECT app.bootstrap_admin('identity-upgrade@example.test','duplicate rollback',
 '$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA');
COMMIT;
SQL
); then bad 'bootstrap accepted duplicate legacy email'
elif [[ $out != *23505* ]]; then bad "bootstrap duplicate email failed for another reason: $out"
elif [[ $(db "$DB" -c "SELECT (SELECT count(*) FROM app.users)||'|'||(SELECT count(*) FROM app.tenants)||'|'||(SELECT count(*) FROM app.tenant_memberships WHERE role='owner')") != '1|1|0' ]]; then bad 'bootstrap duplicate-email rollback left user/tenant/owner'
else pass 'bootstrap duplicate-email rollback preserves legacy user and creates no tenant/owner'; fi
result=$(db "$DB" <<'SQL'
BEGIN ISOLATION LEVEL READ COMMITTED;
SET LOCAL ROLE app_admin_bootstrap;
SELECT app.bootstrap_admin('upgraded-owner@example.test','upgraded first owner',
 '$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA');
COMMIT;
SELECT 'BOOTSTRAP_UPGRADE='||(SELECT count(*) FROM app.users)||'|'||
 (SELECT count(*) FROM app.tenants)||'|'||(SELECT count(*) FROM app.tenant_memberships WHERE role='owner')||'|'||
 EXISTS(SELECT FROM app.tenant_memberships m JOIN app.users u ON u.id=m.user_id JOIN app.tenants t ON t.id=m.tenant_id
  WHERE m.role='owner' AND m.status='active' AND u.email_normalized='upgraded-owner@example.test'
   AND u.status='active' AND t.name='upgraded first owner' AND t.status='active'
   AND u.password_hash='$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA');
SQL
)
if [[ $result == *'BOOTSTRAP_UPGRADE=2|2|1|true'* ]]; then pass 'bootstrap creates first upgraded installation tenant/user/owner through app_admin_bootstrap'
else bad "bootstrap upgraded first-owner witness: $result"; fi
if out=$(db "$DB" -v VERBOSITY=verbose 2>&1 <<'SQL'
BEGIN ISOLATION LEVEL READ COMMITTED;
SET LOCAL ROLE app_admin_bootstrap;
SELECT app.bootstrap_admin('other-owner@example.test','another tenant',
 '$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA');
COMMIT;
SQL
); then bad 'bootstrap created a second installation owner in another tenant'
elif [[ $out != *55000* || $out != *BOOTSTRAP_OWNER_EXISTS* ]]; then bad "bootstrap repeated call failed for another reason: $out"
else pass 'bootstrap rejects second owner globally across tenants'; fi
if out=$(db "$DB" -v VERBOSITY=verbose 2>&1 <<'SQL'
BEGIN ISOLATION LEVEL REPEATABLE READ;
SET LOCAL ROLE app_admin_bootstrap;
SELECT app.bootstrap_admin('rr-owner@example.test','repeatable-read tenant',
 '$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA');
COMMIT;
SQL
); then bad 'bootstrap accepted REPEATABLE READ'
elif [[ $out != *25000* || $out != *BOOTSTRAP_REQUIRES_READ_COMMITTED* ]]; then bad "bootstrap isolation rejection failed for another reason: $out"
else pass 'bootstrap requires READ COMMITTED in a real app_admin_bootstrap transaction'; fi
if [[ $(db "$DB" -c "SELECT (SELECT count(*) FROM app.users)||'|'||(SELECT count(*) FROM app.tenants)||'|'||(SELECT count(*) FROM app.tenant_memberships WHERE role='owner')") != '2|2|1' ]]; then
 bad 'bootstrap rejected repeat/isolation calls left partial user/tenant/owner rows'
fi
db "$DB" -f tests/db_identity_auth.sql >/dev/null
db "$DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $DB"
IDENTITY_DB=""

# C-02b: start immediately before the logical session-activity migration, after
# every predecessor (including identity-auth), then compare all legacy row fields.
login_files=(migrations/[0-9][0-9][0-9]_identity_login.sql)
[[ ${#login_files[@]} -eq 1 && -f ${login_files[0]} ]] || { echo 'FAIL: exactly one identity login migration required'; exit 1; }
LOGIN_DB=upgrade_login_$$
DB=$LOGIN_DB
admin -c "DROP DATABASE IF EXISTS $DB" -c "CREATE DATABASE $DB" 2>/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${login_files[0]} ]] || break
 apply_forward "$DB" "$f"
done
db "$DB" <<'SQL' >/dev/null
INSERT INTO app.users(email,password_hash) VALUES('login-upgrade@example.test','fixture-only') RETURNING id AS u \gset
INSERT INTO app.auth_sessions(user_id,token_hash,created_at,expires_at,revoked_at) VALUES
 (:'u',sha256('legacy-live'::bytea),'2040-01-01Z','2040-01-02Z',NULL),
 (:'u',sha256('legacy-expired'::bytea),'2020-01-01Z','2020-01-02Z',NULL),
 (:'u',sha256('legacy-revoked'::bytea),'2040-01-01Z','2040-01-02Z','2040-01-01T00:01Z');
CREATE TABLE public.login_upgrade_expected AS SELECT id,to_jsonb(s) AS payload FROM app.auth_sessions s;
SQL
apply_forward "$DB" "${login_files[0]}"
result=$(db "$DB" <<'SQL'
SELECT 'LOGIN_UPGRADE='||(SELECT count(*) FROM app.auth_sessions)||'|'||
 (SELECT count(*) FROM app.auth_sessions s JOIN public.login_upgrade_expected e ON e.id=s.id
  AND e.payload=to_jsonb(s)-'last_seen_at')||'|'||
 (SELECT count(*) FROM app.auth_sessions WHERE last_seen_at=created_at)||'|'||
 (SELECT a.attnotnull FROM pg_attribute a WHERE a.attrelid='app.auth_sessions'::regclass
  AND a.attname='last_seen_at' AND NOT a.attisdropped)||'|'||
 has_column_privilege('app_auth','app.auth_sessions','last_seen_at','UPDATE')||'|'||
 has_column_privilege('app_runtime','app.auth_sessions','last_seen_at','UPDATE');
SQL
)
if [[ $result == 'LOGIN_UPGRADE=3|3|3|true|true|false' ]]; then
 pass 'login upgrade preserves old session ids/hashes/expiry/revocation, backfills original creation and restricts activity writes'
else bad "login upgrade witness: $result"; fi
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${login_files[0]} ]] || continue
 apply_forward "$DB" "$f"
done
db "$DB" -f tests/db_identity_login.sql >/dev/null
db "$DB" -f tests/db_identity_auth.sql >/dev/null
db "$DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $DB"
LOGIN_DB=""

# Quote conversion is additive: already-approved immutable rows survive unchanged.
conversions=(migrations/[0-9][0-9][0-9]_quote_conversion.sql)
[[ ${#conversions[@]} == 1 ]] || { echo 'FAIL: exactly one quote conversion migration required' >&2; exit 1; }
CONVERSION_DB=quote_conversion_upgrade_$$
admin -c "CREATE DATABASE $CONVERSION_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${conversions[0]} ]] || break
 apply_forward "$CONVERSION_DB" "$f"
done
# Reuse only the predecessor-compatible fixture before the conversion assertions.
# It builds actual Create/Send/Accept/Approve commands under runtime authority.
{
 awk '/^BEGIN;/{copy=1} /^DO \$conversion\$/{exit} copy' tests/db_quote_conversion.sql
 echo "SELECT pg_temp.qc_quote(); COMMIT;"
} | db "$CONVERSION_DB" >/dev/null
witness_sql="SELECT encode(sha256(jsonb_build_object('id',id,'status',status,'version',version,'snapshot',snapshot,'total',total,'expires_at',expires_at)::text::bytea),'hex') FROM app.quotes;"
before=$(db "$CONVERSION_DB" -c "$witness_sql")
[[ -n $before ]] || bad 'quote conversion upgrade has no approved snapshot witness'
apply_forward "$CONVERSION_DB" "${conversions[0]}"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${conversions[0]} ]] || continue
 apply_forward "$CONVERSION_DB" "$f"
done
result=$(db "$CONVERSION_DB" -c "$witness_sql")
[[ $result == "$before" ]] || bad 'quote conversion migration changed existing approved snapshot'
if ! db "$CONVERSION_DB" < tests/db_quote_conversion.sql >/dev/null || ! db "$CONVERSION_DB" < tests/db_role_matrix.sql >/dev/null; then
 bad 'quote conversion populated upgrade commands or exact role matrix'
else pass 'quote conversion preserves approved snapshot; conversion/expiry/authority and grants after upgrade'; fi
admin -c "DROP DATABASE $CONVERSION_DB" >/dev/null; CONVERSION_DB=""

# H4-clock: discover by suffix and apply every predecessor, never a fixed number.
clock_files=(migrations/[0-9][0-9][0-9]_identity_db_clock.sql)
[[ ${#clock_files[@]} -eq 1 && -f ${clock_files[0]} ]] || { echo 'FAIL: clock migration missing'; exit 1; }
CLOCK_DB=upgrade_clock_$$
admin -c "CREATE DATABASE $CLOCK_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${clock_files[0]} ]] || break
 apply_forward "$CLOCK_DB" "$f"
done
db "$CLOCK_DB" <<'SQL' >/dev/null
INSERT INTO app.users(email,password_hash) VALUES('clock-upgrade@example.test','preserve') RETURNING id AS u \gset
INSERT INTO app.known_devices(user_id,cookie_hash,expires_at) VALUES(:'u',sha256('upgrade-clock'::bytea),clock_timestamp()+interval '1 day');
SELECT app.auth_record_failure('clock-upgrade@example.test','192.0.2.75');
CREATE VIEW public.clock_rows AS
 SELECT 'account' kind,to_jsonb(a) payload FROM app.auth_account_attempts a
 UNION ALL SELECT 'ip',to_jsonb(i) FROM app.auth_login_attempts i
 UNION ALL SELECT 'device',to_jsonb(d) FROM app.known_devices d
 UNION ALL SELECT 'acl',jsonb_build_object('oid',oid,'owner',proowner,'acl',proacl) FROM pg_proc WHERE oid IN ('app.auth_record_failure(text,inet)'::regprocedure,'app.auth_fail_known_device(uuid)'::regprocedure);
CREATE TABLE public.clock_expected AS SELECT * FROM public.clock_rows;
SQL
apply_forward "$CLOCK_DB" "${clock_files[0]}"
[[ $(db "$CLOCK_DB" -c "SELECT NOT EXISTS((TABLE public.clock_rows EXCEPT TABLE public.clock_expected) UNION ALL (TABLE public.clock_expected EXCEPT TABLE public.clock_rows))") == t ]] || bad 'clock upgrade changed stored rows or exact function authority'
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${clock_files[0]} ]] || continue
 apply_forward "$CLOCK_DB" "$f"
done
db "$CLOCK_DB" -f tests/db_identity_clock.sql >/dev/null
db "$CLOCK_DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $CLOCK_DB" >/dev/null; CLOCK_DB=""
pass 'clock upgrade preserves populated attempts/devices and exact function OID/owner/ACL'

# H4: install the password fence over populated identity rows, by suffix.
hardening_files=(migrations/[0-9][0-9][0-9]_hardening.sql)
[[ ${#hardening_files[@]} == 1 && -f ${hardening_files[0]} ]] || { echo 'FAIL: exactly one H4 hardening migration required'; exit 1; }
DB=upgrade_h4_$$
admin -c "CREATE DATABASE $DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${hardening_files[0]} ]] || break
 apply_forward "$DB" "$f"
done
db "$DB" <<'SQL' >/dev/null
INSERT INTO app.users(email,password_hash) VALUES('h4-upgrade@example.test','preserve') RETURNING id AS u \gset
INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES(:'u',sha256('h4-upgrade'::bytea),clock_timestamp()+interval '1 hour');
INSERT INTO app.known_devices(user_id,cookie_hash,expires_at) VALUES(:'u',sha256('h4-device'::bytea),clock_timestamp()+interval '1 day');
CREATE VIEW public.h4_rows AS SELECT 'user' kind,to_jsonb(u) payload FROM app.users u
 UNION ALL SELECT 'session',to_jsonb(s) FROM app.auth_sessions s
 UNION ALL SELECT 'device',to_jsonb(d) FROM app.known_devices d;
CREATE TABLE public.h4_expected AS SELECT * FROM public.h4_rows;
SQL
apply_forward "$DB" "${hardening_files[0]}"
[[ $(db "$DB" -c "SELECT NOT EXISTS((TABLE public.h4_rows EXCEPT TABLE public.h4_expected) UNION ALL (TABLE public.h4_expected EXCEPT TABLE public.h4_rows))") == t ]] || bad 'H4 upgrade changed existing identity rows'
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${hardening_files[0]} ]] || continue
 apply_forward "$DB" "$f"
done
db "$DB" -f tests/db_identity_hardening.sql >/dev/null
db "$DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $DB" >/dev/null
pass 'H4 upgrade preserves users/sessions/devices and enforces password fence with exact ACL'
# ADMIN-PRICING-1: preserve populated lists and tiers across the additive capability.
pricing_staff=(migrations/*_staff_pricing.sql)
[[ ${#pricing_staff[@]} == 1 && -f ${pricing_staff[0]} ]] || { echo 'staff pricing migration missing' >&2; exit 1; }
DB=upgrade_staff_pricing_$$; PRICING_DB=$DB
admin -c "CREATE DATABASE $DB"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${pricing_staff[0]} ]] || continue
 apply_forward "$DB" "$f"
done
db "$DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('legacy-price') RETURNING id AS t \gset
INSERT INTO app.price_lists(tenant_id,currency,starts_at) VALUES(:'t','EUR','2026-01-01T00:00:00Z');
SQL
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${pricing_staff[0]} ]] && continue
 apply_forward "$DB" "$f"
done
[[ $(db "$DB" -c "SELECT currency||'|'||version FROM app.price_lists;") == 'EUR|1' ]] || bad 'staff pricing changed legacy prices'
db "$DB" -f tests/db_staff_pricing.sql >/dev/null
db "$DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $DB"; PRICING_DB=""
pass 'staff pricing upgrade preserves existing currency and initializes CAS version'

# FIX-266: preserve pre-existing listing content/state while adding its own CAS version.
listing_files=(migrations/[0-9][0-9][0-9]_catalog_listing.sql)
[[ ${#listing_files[@]} == 1 ]] || { bad 'exactly one catalog listing migration required'; exit 1; }
LISTING_DB=upgrade_listing_$$; DB=$LISTING_DB
admin -c "CREATE DATABASE $LISTING_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f == "${listing_files[0]}" ]] && break
 apply_forward "$LISTING_DB" "$f"
done
db "$LISTING_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('listing-existing') RETURNING id AS t \gset
SELECT set_config('app.tenant_id',:'t',false);
INSERT INTO app.organizations(tenant_id,display_name) VALUES(:'t','listing') RETURNING id AS o \gset
INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(:'t',:'o','listing') RETURNING id AS s \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','LISTING') RETURNING id AS p \gset
INSERT INTO app.product_content_revisions(tenant_id,product_id,locale,revision,title) VALUES(:'t',:'p','en',7,'Unchanged');
INSERT INTO app.product_listings(tenant_id,site_id,product_id,locale,slug,status,published_content_revision) VALUES(:'t',:'s',:'p','en','unchanged','published',7);
CREATE TABLE public.listing_witness AS SELECT to_jsonb(l) AS snapshot FROM app.product_listings l;
SQL
apply_forward "$LISTING_DB" "${listing_files[0]}"
result=$(db "$LISTING_DB" -c "SELECT (to_jsonb(l)-'version'=w.snapshot) AND l.version=1 FROM app.product_listings l CROSS JOIN public.listing_witness w;")
[[ $result == t ]] || bad "listing upgrade changed content or state: $result"
result=$(db "$LISTING_DB" -c "UPDATE app.product_listings SET slug='updated' RETURNING version;")
[[ $result == 2 ]] || bad "listing legacy write failed to advance version: $result"
admin -c "DROP DATABASE $LISTING_DB" >/dev/null; LISTING_DB=""
pass 'listing upgrade preserves old state and legacy writes advance listing version'

# MDEL-00: apply all predecessors, preserve canonical keys, reject legacy text.
caller_key_files=(migrations/[0-9][0-9][0-9]_caller_key_constraints.sql)
[[ ${#caller_key_files[@]} == 1 ]] || { bad 'exactly one caller key migration required'; exit 1; }
for key_case in canonical invalid; do
 CALLER_KEYS_DB=upgrade_caller_keys_${key_case}_$$
 admin -c "CREATE DATABASE $CALLER_KEYS_DB" >/dev/null
 for f in migrations/[0-9][0-9][0-9]_*.sql; do
  [[ $f == "${caller_key_files[0]}" ]] && break
  apply_forward "$CALLER_KEYS_DB" "$f"
 done
 caller_value=abcdef01-2345-6789-abcd-ef0123456789
 [[ $key_case == canonical ]] || caller_value=alice@example.test
 db "$CALLER_KEYS_DB" <<SQL >/dev/null
 INSERT INTO app.tenants(name) VALUES('caller-key-upgrade') RETURNING id AS t \gset
 INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at)
 VALUES(:'t','service',uuidv7(),'caller-upgrade','$caller_value',sha256('fixture'::bytea),clock_timestamp()+interval '1 hour');
SQL
 if out=$(apply_forward "$CALLER_KEYS_DB" "${caller_key_files[0]}" 2>&1); then
  if [[ $key_case == invalid ]]; then bad 'caller-key upgrade accepted legacy contact text'
  else
   [[ $(db "$CALLER_KEYS_DB" -c "SELECT idempotency_key FROM app.idempotency_records") == "$caller_value" ]] || bad 'caller-key upgrade rewrote identity'
   db "$CALLER_KEYS_DB" -f tests/db_caller_key_constraints.sql >/dev/null
   pass 'caller-key upgrade preserves UUID identity and all deployed column CHECKs reject contact text'
  fi
 elif [[ $key_case == canonical || $out != *idempotency_records_idempotency_key_uuid* ]]; then
  bad "caller-key upgrade unexpected failure: $out"
 else pass 'caller-key upgrade refuses legacy contact text without rewriting historical receipts'
 fi
 admin -c "DROP DATABASE $CALLER_KEYS_DB" >/dev/null; CALLER_KEYS_DB=""
done

# MDEL-01: a populated predecessor database and role grants survive unchanged.
# The migration's suffix is stable when integration assigns its formal number.
lifecycle_files=(migrations/[0-9][0-9][0-9]_tenant_lifecycle.sql)
[[ ${#lifecycle_files[@]} == 1 ]] || { bad 'exactly one lifecycle migration required'; exit 1; }
LIFECYCLE_DB=upgrade_lifecycle_$$
admin -c "CREATE DATABASE $LIFECYCLE_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f == "${lifecycle_files[0]}" ]] && break
 apply_forward "$LIFECYCLE_DB" "$f"
done
db "$LIFECYCLE_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name,status) VALUES('mdel01-upgrade-active','active'),('mdel01-upgrade-suspended','suspended');
CREATE TABLE public.mdel01_scope_before AS
 SELECT role,app.auth_role_scopes(role) scopes FROM unnest(ARRAY['owner','admin','catalog_editor','content_editor','viewer','unknown','*','']) role;
SQL
lifecycle_before=$(db "$LIFECYCLE_DB" -c 'SELECT jsonb_agg(to_jsonb(t) ORDER BY id)::text FROM app.tenants t;')
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${lifecycle_files[0]} ]] && continue
 apply_forward "$LIFECYCLE_DB" "$f"
done
lifecycle_after=$(db "$LIFECYCLE_DB" -c 'SELECT jsonb_agg(to_jsonb(t) ORDER BY id)::text FROM app.tenants t;')
[[ -n $lifecycle_before && $lifecycle_before == "$lifecycle_after" ]] || bad 'MDEL01_UPGRADE_CHANGED_TENANTS'
db "$LIFECYCLE_DB" <<'SQL' >/dev/null
DO $gate$ BEGIN
 IF EXISTS(SELECT FROM public.mdel01_scope_before WHERE NOT scopes <@ app.auth_role_scopes(role)) THEN
  RAISE EXCEPTION 'MDEL01_SCOPE_REGRESSION'; END IF;
 IF EXISTS(SELECT FROM public.mdel01_scope_before WHERE ('tenant:delete'=ANY(app.auth_role_scopes(role))) IS DISTINCT FROM (role='owner')) THEN
  RAISE EXCEPTION 'MDEL01_SCOPE_NOT_OWNER_ONLY'; END IF;
 IF EXISTS(SELECT FROM app.tenants t LEFT JOIN app.tenant_lifecycles l ON l.tenant_id=t.id AND l.id=t.id
  WHERE l.tenant_id IS NULL OR l.state<>CASE WHEN t.status='deleted' THEN 'deleted' ELSE 'active' END OR l.epoch<>0) THEN
  RAISE EXCEPTION 'MDEL01_UPGRADE_BACKFILL'; END IF;
END $gate$;
DROP TABLE public.mdel01_scope_before;
SQL
db "$LIFECYCLE_DB" -f tests/db_tenant_lifecycle.sql >/dev/null
db "$LIFECYCLE_DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $LIFECYCLE_DB" >/dev/null; LIFECYCLE_DB=""
pass 'MDEL01 populated upgrade preserves business rows and every prior role scope'

# Aggregate only after all module upgrade steps.
[[ $fail -eq 0 ]] || exit 1
# Quotes takes over the existing005 inquiry table without rebuilding contacts.
# Filename order applies every predecessor, including any integrator renumbering.
quotes=(migrations/[0-9][0-9][0-9]_quotes.sql)
[[ ${#quotes[@]} == 1 ]] || { echo 'FAIL: exactly one quotes migration required' >&2; exit 1; }
QUOTES_DB=quotes_upgrade_$$
admin -c "CREATE DATABASE $QUOTES_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${quotes[0]} ]] || break
 apply_forward "$QUOTES_DB" "$f"
done
db "$QUOTES_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('quote-existing') RETURNING id AS t \gset
INSERT INTO app.organizations(tenant_id,display_name) VALUES(:'t','legacy') RETURNING id AS o \gset
INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(:'t',:'o','legacy') RETURNING id AS s \gset
INSERT INTO app.inquiries(tenant_id,site_id,buyer_email,buyer_name,message,status)
VALUES(:'t',:'s','legacy@example.invalid','Legacy contact','preserve this inquiry','contacted');
SQL
apply_forward "$QUOTES_DB" "${quotes[0]}"
result=$(db "$QUOTES_DB" -c "SELECT 'QUOTES_UPGRADE='||count(*) FROM app.inquiries WHERE buyer_email='legacy@example.invalid' AND buyer_name='Legacy contact' AND message='preserve this inquiry' AND status='contacted' AND buyer_company_id IS NULL;")
[[ $result == 'QUOTES_UPGRADE=1' ]] || bad "quotes existing inquiry contact preservation: $result"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${quotes[0]} ]] || continue
 apply_forward "$QUOTES_DB" "$f"
done
river_db "$QUOTES_DB" --verify
if ! db "$QUOTES_DB" < tests/db_quotes.sql >/dev/null || ! db "$QUOTES_DB" < tests/db_role_matrix.sql >/dev/null; then
 bad 'quotes lifecycle and exact role matrix after populated inquiry upgrade'
else pass 'quotes forward ALTER preserves existing contact inquiry and runtime lifecycle/grants'; fi
admin -c "DROP DATABASE $QUOTES_DB" >/dev/null; QUOTES_DB=""


# Controlled CMS upgrade preserves populated legacy drafts/releases and adds
# capability-fenced managed drafts without rewriting predecessor migrations.
cms=(migrations/[0-9][0-9][0-9]_cms_publish.sql)
[[ ${#cms[@]} == 1 ]] || { echo 'FAIL: exactly one CMS publish migration required' >&2; exit 1; }
CMS_DB=cms_upgrade_$$
admin -c "CREATE DATABASE $CMS_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${cms[0]} ]] || break
 apply_forward "$CMS_DB" "$f"
done
db "$CMS_DB" < tests/fixtures/storefront_seed.sql >/dev/null
before=$(db "$CMS_DB" -c "SELECT count(*)||'|'||encode(sha256(convert_to(jsonb_agg(jsonb_build_array(tenant_id,site_id,id,base_release_id,revision,status) ORDER BY id)::text,'UTF8')),'hex') FROM app.site_drafts;")
if red=$(db "$CMS_DB" < tests/db_cms_publish.sql 2>&1); then bad 'CMS gates passed before controlled capability migration'
elif [[ $red == *'function app.cms_create_draft'* && $red == *'does not exist'* ]]; then pass 'CMS pre-migration red: controlled function capability absent'
else bad "CMS unexpected pre-migration red: $red"; fi
apply_forward "$CMS_DB" "${cms[0]}"
after=$(db "$CMS_DB" -c "SELECT count(*)||'|'||encode(sha256(convert_to(jsonb_agg(jsonb_build_array(tenant_id,site_id,id,base_release_id,revision,status) ORDER BY id)::text,'UTF8')),'hex') FROM app.site_drafts;")
[[ $before == "$after" ]] || bad 'CMS upgrade changed legacy draft identities/revisions/status/base'
result=$(db "$CMS_DB" -c "SELECT count(*) FROM app.site_drafts WHERE is_cms;")
[[ $result == 0 ]] || bad 'CMS upgrade silently adopted unmanaged drafts'
for f in migrations/[0-9][0-9][0-9]_*.sql; do [[ $f > ${cms[0]} ]] || continue; apply_forward "$CMS_DB" "$f"; done
if ! db "$CMS_DB" < tests/db_cms_publish.sql >/dev/null || ! db "$CMS_DB" < tests/db_storefront.sql >/dev/null || ! db "$CMS_DB" < tests/db_role_matrix.sql >/dev/null; then
 bad 'CMS populated upgrade controlled/CAS/freeze/role/public-read gate failed'
else pass 'CMS upgrade preserves legacy state; controlled writes/immutable history and public role gates pass'; fi
admin -c "DROP DATABASE $CMS_DB" >/dev/null; CMS_DB=""


# Checkout installs after EVERY prior forward migration; numeric allocation is
# deliberately irrelevant. Legacy paid orders and their outbox are not rewritten
# (created + paid + notifications fan-out email = 3 events).
checkout_files=(migrations/[0-9][0-9][0-9]_checkout.sql)
[[ ${#checkout_files[@]} == 1 ]] || { echo 'FAIL: exactly one checkout migration required'; exit 1; }
CHECKOUT_DB=upgrade_checkout_$$
admin -c "DROP DATABASE IF EXISTS $CHECKOUT_DB" -c "CREATE DATABASE $CHECKOUT_DB" 2>/dev/null
river_db "$CHECKOUT_DB"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${checkout_files[0]} ]] || break
 apply_forward "$CHECKOUT_DB" "$f"
done
db "$CHECKOUT_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('checkout-legacy') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','checkout-legacy') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'p','checkout-legacy','default') RETURNING id AS sku \gset
SELECT set_config('app.tenant_id',:'t',false);
SET ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',2,'legacy stock',uuidv7(),'inbound');
RESET ROLE; SET ROLE app_runtime;
SELECT uuidv7() AS oid \gset
SELECT (app.inventory_reserve(:'sku',1,'order',:'oid','9c45e55c-400f-5b91-a5f5-36201bb37bd4',900)).id AS rid \gset
SELECT app.orders_create(:'oid','417f5891-373e-50d7-8b30-21e3aa2dfe53',sha256('legacy'::bytea),NULL,'{"email":"legacy@example.invalid"}','USD',0,'{}',
 jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',:'sku','sku','checkout-legacy','quantity',1,'unit_price_minor',100,
 'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,'reservation_id',:'rid','tax_snapshot','{"category":"standard"}'::jsonb)),'[]');
SELECT app.orders_transition(:'oid','paid','legacy-paid',0);
SQL
apply_forward "$CHECKOUT_DB" "${checkout_files[0]}"
result=$(db "$CHECKOUT_DB" -c "SELECT o.state||'|'||o.version||'|'||o.total_minor||'|'||s.on_hand||'|'||s.reserved||'|'||(SELECT count(*) FROM app.outbox_events)||'|'||(SELECT count(*) FROM app.checkout_sessions)||'|'||(SELECT count(*) FROM app.checkout_attempts) FROM app.orders o JOIN app.stock_levels s USING(tenant_id);")
[[ $result == 'paid|2|100|1|0|3|0|0' ]] || bad "checkout populated upgrade witness: $result"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${checkout_files[0]} ]] || continue
 apply_forward "$CHECKOUT_DB" "$f"
done
river_db "$CHECKOUT_DB" --verify
db "$CHECKOUT_DB" -f tests/db_checkout.sql >/dev/null
db "$CHECKOUT_DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $CHECKOUT_DB"; CHECKOUT_DB=""
pass 'checkout upgrade preserves paid legacy orders/inventory/outbox and installs permanent fenced receipts'
# Customer auth upgrades every actual predecessor; old credentials and immutable
# guest snapshots survive. Mailbox proof verifies legacy identity, never changes PHC.
customer_auth=(migrations/[0-9][0-9][0-9]_customer_auth.sql)
[[ ${#customer_auth[@]} == 1 ]] || { echo 'FAIL: exactly one customer auth migration required' >&2; exit 1; }
CUSTOMER_AUTH_DB=customer_auth_upgrade_$$
admin -c "CREATE DATABASE $CUSTOMER_AUTH_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${customer_auth[0]} ]] || break
 apply_forward "$CUSTOMER_AUTH_DB" "$f"
done
db "$CUSTOMER_AUTH_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('customer-auth-legacy') RETURNING id AS t \gset
INSERT INTO app.customers(tenant_id,email,display_name) VALUES(:'t','legacy-auth@example.invalid','Legacy') RETURNING id AS c \gset
INSERT INTO app.customer_credentials(tenant_id,customer_id,password_hash) VALUES(:'t',:'c',
 '$argon2id$v=19$m=19456,t=2,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU');
SELECT set_config('app.tenant_id',:'t',false);
SET ROLE app_orders_executor;
INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
VALUES(:'t',uuidv7(),1,'5fbc3309-f14e-5aec-b745-2596a5083c2b',sha256('auth-upgrade'::bytea),'{"email":"legacy-auth@example.invalid","name":"Immutable"}','USD',0,0,0,'{}',0) RETURNING id AS oid \gset
UPDATE app.orders SET sealed=true WHERE tenant_id=:'t' AND id=:'oid';
RESET ROLE;
CREATE TABLE public.customer_auth_upgrade_witness AS
 SELECT to_jsonb(c) AS customer,to_jsonb(p) AS credential,to_jsonb(o) AS snapshot
 FROM app.customers c JOIN app.customer_credentials p ON(p.tenant_id,p.customer_id)=(c.tenant_id,c.id)
 JOIN app.orders o ON o.tenant_id=c.tenant_id WHERE c.id=:'c';
SQL
# An inbound membership must refuse the new role before any additive schema lands.
admin -c 'GRANT app_customer_executor TO app_runtime' >/dev/null
if output=$(apply_forward "$CUSTOMER_AUTH_DB" "${customer_auth[0]}" 2>&1); then
 bad 'customer executor inbound membership unexpectedly accepted'
else
 [[ $output == *CUSTOMER_AUTH_EXECUTOR_UNSAFE* ]] || bad "customer executor membership wrong failure: $output"
fi
admin -c 'REVOKE app_customer_executor FROM app_runtime' >/dev/null
result=$(db "$CUSTOMER_AUTH_DB" -c "SELECT 'AUTH_ATOMIC='||(to_regclass('app.customer_auth_tokens') IS NULL)||'|'||(NOT EXISTS(SELECT FROM pg_attribute WHERE attrelid='app.customers'::regclass AND attname='verified_at' AND NOT attisdropped));")
[[ $result == 'AUTH_ATOMIC=true|true' ]] || bad "customer executor refused migration left partial schema: $result"
apply_forward "$CUSTOMER_AUTH_DB" "${customer_auth[0]}"
result=$(db "$CUSTOMER_AUTH_DB" <<'SQL'
SELECT 'AUTH_UPGRADE='||(to_jsonb(c)-'verified_at'=w.customer)||'|'||(c.verified_at IS NULL)||'|'||(to_jsonb(p)=w.credential)||'|'||(to_jsonb(o)=w.snapshot)
 FROM public.customer_auth_upgrade_witness w JOIN app.customers c ON c.id=(w.customer->>'id')::uuid
 JOIN app.customer_credentials p ON(p.tenant_id,p.customer_id)=(c.tenant_id,c.id)
 JOIN app.orders o ON o.id=(w.snapshot->>'id')::uuid;
DO $proof$
DECLARE t uuid; c uuid; legacy_phc text; candidate text; BEGIN
 SELECT x.tenant_id,x.id,p.password_hash INTO STRICT t,c,legacy_phc FROM app.customers x
 JOIN app.customer_credentials p ON(p.tenant_id,p.customer_id)=(x.tenant_id,x.id) WHERE x.email='legacy-auth@example.invalid';
 candidate:=replace(legacy_phc,'YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU','ZWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU');
 PERFORM set_config('app.tenant_id',t::text,true);
 EXECUTE 'SET LOCAL ROLE app_customer_auth';
 IF NOT app.customer_prepare_registration('legacy-auth@example.invalid',candidate,sha256('legacy-proof'::bytea),clock_timestamp()+interval '1 hour')
 OR NOT app.customer_consume_verification(sha256('legacy-proof'::bytea)) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_UPGRADE_PROOF'; END IF;
 IF (SELECT password_hash FROM app.customer_credentials WHERE tenant_id=t AND customer_id=c)<>legacy_phc
 OR NOT EXISTS(SELECT FROM app.customer_guest_order_links WHERE tenant_id=t AND customer_id=c) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_UPGRADE_LEGACY_CHANGED'; END IF;
 IF NOT app.customer_prepare_reset('legacy-auth@example.invalid',sha256('legacy-reset'::bytea),clock_timestamp()+interval '1 hour')
 OR NOT app.customer_consume_reset(sha256('legacy-reset'::bytea),candidate) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_UPGRADE_RESET'; END IF;
 EXECUTE 'RESET ROLE';
 IF (SELECT to_jsonb(o) FROM app.orders o WHERE o.id=(SELECT (snapshot->>'id')::uuid FROM public.customer_auth_upgrade_witness))
 IS DISTINCT FROM(SELECT snapshot FROM public.customer_auth_upgrade_witness) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_UPGRADE_ORDER_CHANGED'; END IF;
END $proof$;
SQL
)
[[ $result == *'AUTH_UPGRADE=true|true|true|true'* ]] || bad "customer populated upgrade preservation: $result"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${customer_auth[0]} ]] || continue
 apply_forward "$CUSTOMER_AUTH_DB" "$f"
done
river_db "$CUSTOMER_AUTH_DB" --verify
db "$CUSTOMER_AUTH_DB" -f tests/db_customer_auth.sql >/dev/null
db "$CUSTOMER_AUTH_DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $CUSTOMER_AUTH_DB" >/dev/null; CUSTOMER_AUTH_DB=""
pass 'customer auth forward upgrade preserves legacy PHC/guest snapshot, rejects unsafe role atomically, then verifies and resets explicitly'

# C-20b: legacy non-order holds have no bearer. Discover the logical migration
# and apply every predecessor, including River authorization when encountered.
# No migration number or history count is part of this populated upgrade gate.
inventory_owner=(migrations/[0-9][0-9][0-9]_inventory_hold_owner.sql)
[[ ${#inventory_owner[@]} == 1 && -f ${inventory_owner[0]} ]] || { bad 'exactly one inventory hold owner migration required'; exit 1; }
INVENTORY_OWNER_DB=inventory_owner_upgrade_$$
admin -c "CREATE DATABASE $INVENTORY_OWNER_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${inventory_owner[0]} ]] || break
 apply_forward "$INVENTORY_OWNER_DB" "$f"
done
db "$INVENTORY_OWNER_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('inventory-owner-upgrade') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','legacy-holder') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 VALUES(:'t',:'p','legacy-holder','default') RETURNING id AS sku \gset
SELECT uuidv7() AS owner \gset
SELECT set_config('app.tenant_id',:'t',false);
SET ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',2,'legacy holder receipt',uuidv7(),'inbound');
RESET ROLE;
SET ROLE app_runtime;
SELECT (app.inventory_reserve(:'sku',1,'checkout',:'owner','0ea3a358-53cd-50a6-aa5b-9d55a7be7c5e',900)).id AS rid \gset
RESET ROLE;
CREATE TABLE public.inventory_owner_upgrade_witness AS
 SELECT to_jsonb(r) AS snapshot FROM app.inventory_reservations r WHERE r.id=:'rid';
SQL
apply_forward "$INVENTORY_OWNER_DB" "${inventory_owner[0]}"
result=$(db "$INVENTORY_OWNER_DB" <<'SQL'
DO $proof$
DECLARE legacy app.inventory_reservations; new_hold app.inventory_reservations;
 bearer bytea:=sha256((gen_random_uuid()::text||gen_random_uuid()::text)::bytea); denied boolean;
BEGIN
 SELECT r.* INTO STRICT legacy FROM app.inventory_reservations r
 WHERE r.id=(SELECT (snapshot->>'id')::uuid FROM public.inventory_owner_upgrade_witness);
 IF legacy.holder_hash IS NOT NULL OR legacy.state<>'active'
 OR (to_jsonb(legacy)-'holder_hash') IS DISTINCT FROM (SELECT snapshot FROM public.inventory_owner_upgrade_witness)
 THEN RAISE EXCEPTION 'INVENTORY_HOLDER_UPGRADE_LEGACY_CHANGED'; END IF;
 PERFORM set_config('app.tenant_id',legacy.tenant_id::text,false);
 EXECUTE 'SET LOCAL ROLE app_runtime';
 denied:=false;
 BEGIN PERFORM app.inventory_release_non_order(legacy.id,legacy.owner_type,legacy.owner_id,bearer);
 EXCEPTION WHEN SQLSTATE '42501' THEN denied:=true; END;
 IF NOT denied THEN RAISE EXCEPTION 'INVENTORY_HOLDER_UPGRADE_LEGACY_RELEASE_ACCEPTED'; END IF;
 denied:=false;
 BEGIN PERFORM app.inventory_commit_non_order(legacy.id,legacy.owner_type,legacy.owner_id,bearer);
 EXCEPTION WHEN SQLSTATE '42501' THEN denied:=true; END;
 IF NOT denied THEN RAISE EXCEPTION 'INVENTORY_HOLDER_UPGRADE_LEGACY_COMMIT_ACCEPTED'; END IF;
 denied:=false;
 BEGIN PERFORM app.inventory_expire_due(100);
 EXCEPTION WHEN SQLSTATE '42501' THEN denied:=true; END;
 IF NOT denied THEN RAISE EXCEPTION 'INVENTORY_HOLDER_UPGRADE_RUNTIME_SWEEP_ACCEPTED'; END IF;
 EXECUTE 'RESET ROLE';
 EXECUTE 'SET LOCAL ROLE app_queue_worker';
 IF app.inventory_expire_due(100)<>0 THEN RAISE EXCEPTION 'INVENTORY_HOLDER_UPGRADE_EARLY_EXPIRE'; END IF;
 EXECUTE 'RESET ROLE';
 UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE id=legacy.id;
 EXECUTE 'SET LOCAL ROLE app_queue_worker';
 IF app.inventory_expire_due(100)<>1 THEN RAISE EXCEPTION 'INVENTORY_HOLDER_UPGRADE_LEGACY_SWEEP'; END IF;
 EXECUTE 'RESET ROLE';
 EXECUTE 'SET LOCAL ROLE app_runtime';
 new_hold:=app.inventory_reserve_non_order(legacy.sku_id,1,'checkout',legacy.owner_id,'a9c1e8e0-d99b-53ca-b238-2fdeed06d2cd',900,bearer);
 new_hold:=app.inventory_commit_non_order(new_hold.id,'checkout',legacy.owner_id,bearer);
 IF new_hold.state<>'committed' OR new_hold.holder_hash IS DISTINCT FROM sha256(bearer)
 THEN RAISE EXCEPTION 'INVENTORY_HOLDER_UPGRADE_NEW_CAPABILITY'; END IF;
 -- Authorized terminal replay must not add a second movement.
 PERFORM app.inventory_commit_non_order(new_hold.id,'checkout',legacy.owner_id,bearer);
 EXECUTE 'RESET ROLE';
 IF (SELECT state FROM app.inventory_reservations WHERE id=legacy.id)<>'expired'
 OR (SELECT count(*) FROM app.inventory_movements WHERE reservation_id=legacy.id AND kind='expire')<>1
 OR (SELECT count(*) FROM app.inventory_movements WHERE reservation_id=new_hold.id AND kind='commit')<>1
 THEN RAISE EXCEPTION 'INVENTORY_HOLDER_UPGRADE_TERMINAL_WITNESS'; END IF;
END $proof$;
SELECT 'HOLDER_UPGRADE='||s.on_hand||'|'||s.reserved||'|'||
 (SELECT count(*) FROM app.inventory_movements m WHERE m.tenant_id=s.tenant_id)||'|'||
 (SELECT count(*) FROM app.inventory_reservations r WHERE r.tenant_id=s.tenant_id AND r.state='active')
 FROM app.stock_levels s JOIN app.tenants t ON t.id=s.tenant_id WHERE t.name='inventory-owner-upgrade';
SQL
)
[[ $result == *'HOLDER_UPGRADE=1|0|5|0'* ]] || bad "inventory holder populated upgrade: $result"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${inventory_owner[0]} ]] || continue
 apply_forward "$INVENTORY_OWNER_DB" "$f"
done
river_db "$INVENTORY_OWNER_DB" --verify
db "$INVENTORY_OWNER_DB" -f tests/db_inventory.sql >/dev/null
db "$INVENTORY_OWNER_DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $INVENTORY_OWNER_DB" >/dev/null; INVENTORY_OWNER_DB=""
pass 'inventory holder upgrade preserves legacy hold, refuses owner finish, expires only via due queue authority and accepts new bearer'


# Storefront serving: a site published before the migration keeps its pointer and gets
# no serving row (the public read serves its legacy projection until republish).
serving_files=(migrations/[0-9][0-9][0-9]_storefront_serving.sql)
[[ ${#serving_files[@]} == 1 ]] || { echo 'FAIL: exactly one storefront serving migration required'; exit 1; }
SERVING_DB=upgrade_serving_$$
admin -c "DROP DATABASE IF EXISTS $SERVING_DB" -c "CREATE DATABASE $SERVING_DB" 2>/dev/null
river_db "$SERVING_DB"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${serving_files[0]} ]] || break
 apply_forward "$SERVING_DB" "$f"
done
db "$SERVING_DB" < tests/fixtures/storefront_seed.sql >/dev/null
apply_forward "$SERVING_DB" "${serving_files[0]}"
result=$(db "$SERVING_DB" -c "SELECT 'SERVING_UPGRADE='||(SELECT count(*) FROM app.sites WHERE published_release_id IS NOT NULL)||'|'||(SELECT count(*) FROM app.storefront_serving)||'|'||(SELECT count(*) FROM app.site_release_skus)")
[[ $result == *'SERVING_UPGRADE=2|0|0'* ]] || bad "storefront serving populated upgrade witness: $result"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${serving_files[0]} ]] || continue
 apply_forward "$SERVING_DB" "$f"
done
river_db "$SERVING_DB" --verify
if ! db "$SERVING_DB" < tests/db_storefront.sql >/dev/null || ! db "$SERVING_DB" < tests/db_storefront_serving.sql >/dev/null || ! db "$SERVING_DB" < tests/db_role_matrix.sql >/dev/null; then
 bad 'storefront serving RLS/role matrix after populated upgrade'
else pass 'storefront serving upgrade keeps published sites and adds an empty serving projection'; fi
admin -c "DROP DATABASE $SERVING_DB" >/dev/null; SERVING_DB=""
# Signed payment facts are additive to existing ready checkouts. Preserve their
# payable/provider identity and default refund accounting to zero, by suffix.
payment_files=(migrations/[0-9][0-9][0-9]_payment_webhooks.sql)
[[ ${#payment_files[@]} == 1 ]] || { echo 'FAIL: exactly one payment webhook migration required'; exit 1; }
PAYMENTS_DB=upgrade_payments_$$
admin -c "CREATE DATABASE $PAYMENTS_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${payment_files[0]} ]] || break
 apply_forward "$PAYMENTS_DB" "$f"
done
db "$PAYMENTS_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('payment-upgrade') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','payment-upgrade') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'p','payment-upgrade','default') RETURNING id AS sku \gset
SELECT set_config('app.tenant_id',:'t',false);
SET ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',2,'payment upgrade stock',uuidv7(),'inbound');
SET ROLE app_runtime;
SELECT uuidv7() AS oid \gset
SELECT (app.inventory_reserve(:'sku',1,'order',:'oid','fe185518-52b1-50e1-9c69-81e76eb28d41',900)).id AS rid \gset
SELECT app.orders_create(:'oid','da30010d-e0e7-511e-ac42-a3bcd85501be',sha256('payment-upgrade'::bytea),NULL,'{"email":"payment-upgrade@example.invalid"}','USD',0,'{}',
 jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',:'sku','sku','payment-upgrade','quantity',1,'unit_price_minor',100,
 'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,'reservation_id',:'rid','tax_snapshot','{"category":"standard"}'::jsonb)),'[]');
INSERT INTO app.checkout_sessions(tenant_id,id,actor_kind,actor_id,request_key,request_hash,cart_id,cart_version,order_id,address_snapshot,currency,amount_minor,items_net_minor,tax_minor,shipping_minor,payable_version)
VALUES(:'t',uuidv7(),'guest',repeat('a',64),'6c045ad4-5d06-55f1-a725-cf7dadb256a6',sha256('upgrade'::bytea),uuidv7(),1,:'oid','{}','USD',100,100,0,0,1);
UPDATE app.checkout_sessions SET phase='ready',provider_ref='fake_upgrade',start_result='{}';
RESET ROLE;
CREATE TABLE public.payment_upgrade_witness AS SELECT to_jsonb(s) AS snapshot FROM app.checkout_sessions s;
SQL
apply_forward "$PAYMENTS_DB" "${payment_files[0]}"
result=$(db "$PAYMENTS_DB" -c "SELECT (to_jsonb(s)-ARRAY['refunded_net_minor','refunded_tax_minor','refunded_shipping_minor']=w.snapshot) AND s.refunded_net_minor=0 AND s.refunded_tax_minor=0 AND s.refunded_shipping_minor=0 FROM app.checkout_sessions s CROSS JOIN public.payment_upgrade_witness w;")
[[ $result == t ]] || bad "payment ready checkout upgrade changed identity: $result"
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f > ${payment_files[0]} ]] || continue
 apply_forward "$PAYMENTS_DB" "$f"
done
db "$PAYMENTS_DB" -f tests/db_payments.sql >/dev/null
db "$PAYMENTS_DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $PAYMENTS_DB" >/dev/null; PAYMENTS_DB=""
pass 'payment upgrade preserves ready checkout snapshot and defaults bounded refund counters'
# C-20a: preserve a pre-module paid/committed order, then allocate shipment
# and compensate its unshipped remainder. Module identity follows filename suffix.
fulfillment_files=(migrations/[0-9][0-9][0-9]_fulfillment_returns.sql)
[[ ${#fulfillment_files[@]} == 1 ]] || { bad 'exactly one fulfillment migration required'; exit 1; }
FULFILLMENT_DB=upgrade_fulfillment_$$; DB=$FULFILLMENT_DB
admin -c "CREATE DATABASE $DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${fulfillment_files[0]} ]] || break
 apply_forward "$DB" "$f"
done
db "$DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('fulfillment-existing') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','existing') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'p','existing','default') RETURNING id AS sku \gset
SELECT set_config('app.tenant_id',:'t',false);
SET ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',5,'legacy stock',uuidv7(),'inbound');
SET ROLE app_runtime;
SELECT uuidv7() AS oid,uuidv7() AS lid \gset
SELECT (app.inventory_reserve(:'sku',2,'order',:'oid','149d0bb7-c269-5a68-9633-7e405a354dba',900)).id AS rid \gset
SELECT app.orders_create(:'oid','53e0d56a-c6e9-5777-8fa9-abab6e8e3b68',sha256('existing'::bytea),NULL,'{"email":"existing@example.invalid"}',
 'USD',2,'{"amount_minor":2,"currency":"USD","service":"fixture"}',
 jsonb_build_array(jsonb_build_object('id',:'lid','sku_id',:'sku','sku','existing','quantity',2,'unit_price_minor',5,
 'line_net_minor',10,'line_tax_minor',2,'line_total_minor',12,'reservation_id',:'rid','tax_snapshot','{"category":"standard"}'::jsonb)),'[]');
SELECT app.orders_transition(:'oid','paid','existing-paid',0);
SQL
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${fulfillment_files[0]} ]] && continue
 apply_forward "$DB" "$f"
done
result=$(db "$DB" <<'SQL'
SELECT id AS t FROM app.tenants WHERE name='fulfillment-existing' \gset
SELECT id AS oid FROM app.orders WHERE tenant_id=:'t' \gset
SELECT id AS lid FROM app.order_lines WHERE tenant_id=:'t' \gset
SELECT set_config('app.tenant_id',:'t',false);
SET ROLE app_runtime;
SELECT app.fulfillment_create_shipment(:'oid',2,'3c936835-b249-5597-a6fc-0ffad7bdcde1','manual','LEGACY',
 jsonb_build_array(jsonb_build_object('order_line_id',:'lid','quantity',1)));
SELECT app.returns_create(:'oid',3,'51421e49-54c0-57f1-af3b-8926b4215af5','unshipped_cancel','legacy remainder',
 jsonb_build_array(jsonb_build_object('order_line_id',:'lid','quantity',1)));
SELECT 'FULFILLMENT_UPGRADE='||o.state||'|'||o.version||'|'||s.on_hand||'|'||s.reserved||'|'||
 (SELECT sum(quantity) FROM app.shipment_lines WHERE tenant_id=:'t')||'|'||
 (SELECT refund_minor FROM app.returns WHERE tenant_id=:'t')||'|'||
 (SELECT count(*) FROM app.inventory_movements WHERE tenant_id=:'t' AND owner_type='return')
 FROM app.orders o JOIN app.stock_levels s USING(tenant_id) WHERE o.tenant_id=:'t';
SQL
)
[[ $result == *'FULFILLMENT_UPGRADE=partially_fulfilled|4|4|0|1|7|1'* ]] || bad "fulfillment paid legacy upgrade: $result"
db "$DB" -f tests/db_fulfillment_returns.sql >/dev/null
db "$DB" -f tests/db_role_matrix.sql >/dev/null
# C-19d preparation: all predecessors above include populated pre-compensation
# orders. Do not invent provider/journal bindings during the forward upgrade.
db "$DB" -f tests/db_refund_compensation.sql >/dev/null
result=$(db "$DB" -c "SELECT (SELECT count(*) FROM app.payment_refund_attempts)||'|'||(SELECT count(*) FROM app.payment_refund_reversals)")
[[ $result == *'0|0'* ]] || bad "compensation preparation backfilled unproven legacy identities: $result"
admin -c "DROP DATABASE $DB"; FULFILLMENT_DB=""
pass 'fulfillment upgrade preserves paid inventory and compensates only unshipped quantities'


# C19d3: populated pre-recovery quarantine is imported without guessing money.
recovery_files=(migrations/[0-9][0-9][0-9]_refund_reconciliation.sql)
[[ ${#recovery_files[@]} == 1 ]] || { bad 'exactly one reconciliation migration required'; exit 1; }
RECOVERY_DB=upgrade_refund_recovery_$$
admin -c "CREATE DATABASE $RECOVERY_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${recovery_files[0]} ]] || break
 apply_forward "$RECOVERY_DB" "$f"
done
db "$RECOVERY_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('recovery-upgrade') RETURNING id AS t \gset
INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment)
 VALUES(:'t','stripe','acct_recovery_upgrade','test');
INSERT INTO app.webhook_inbox(tenant_id,provider,provider_account_id,environment,provider_event_id,payload_hash,payload_reference,status)
 VALUES(:'t','stripe','acct_recovery_upgrade','test','evt_recovery_upgrade',sha256('retained-only'::bytea),'payment-event:retained','failed');
SQL
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${recovery_files[0]} ]] && continue
 apply_forward "$RECOVERY_DB" "$f"
done
result=$(db "$RECOVERY_DB" -c "SELECT count(*)||'|'||bool_and(c.provider_refund_id IS NULL AND c.provider_ref IS NULL AND c.evidence_hash=i.payload_hash)||'|'||(SELECT count(*) FROM app.payment_refund_case_resolutions)||'|'||(SELECT count(*) FROM app.payment_refund_reversals) FROM app.payment_refund_cases c JOIN app.payment_refund_case_deliveries d ON(d.tenant_id,d.case_id)=(c.tenant_id,c.id) JOIN app.webhook_inbox i ON(i.tenant_id,i.id)=(d.tenant_id,d.inbox_id);")
[[ $result == '1|true|0|0' ]] || bad "C19D3_UPGRADE_QUARANTINE $result"
db "$RECOVERY_DB" -f tests/db_role_matrix.sql >/dev/null
db "$RECOVERY_DB" -f tests/db_refund_recovery.sql >/dev/null
admin -c "DROP DATABASE $RECOVERY_DB" >/dev/null; RECOVERY_DB=""
pass 'reconciliation upgrade preserves original Inbox evidence without invented bindings or money'

# Privacy forward upgrade must refuse legacy guest tombstones whose typed proof
# has already been deleted; reclassifying arbitrary JSON would invent ownership.
privacy_bounds=(migrations/[0-9][0-9][0-9]_privacy_bounds.sql)
[[ ${#privacy_bounds[@]} == 1 && -f ${privacy_bounds[0]} ]] || { bad 'exactly one privacy bounds migration required'; exit 1; }
PRIVACY_HARDENING_DB=privacy_hardening_upgrade_$$
admin -c "CREATE DATABASE $PRIVACY_HARDENING_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${privacy_bounds[0]} ]] || break
 apply_forward "$PRIVACY_HARDENING_DB" "$f"
done
db "$PRIVACY_HARDENING_DB" <<'SQL' >/dev/null
BEGIN;
INSERT INTO app.tenants(name) VALUES('privacy-legacy-guest') RETURNING id AS t \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'t','legacy-guest@example.invalid') RETURNING id AS c \gset
SELECT set_config('app.tenant_id',:'t',true);
SET LOCAL ROLE app_orders_executor;
INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
 VALUES(:'t',uuidv7(),1,'cd4d4fb9-c7ae-5881-8fe9-fd4aa2422927',sha256('legacy'::bytea),'{"email":"legacy-guest@example.invalid"}','USD',100,0,0,'{}',100) RETURNING id AS o \gset
RESET ROLE;
INSERT INTO app.customer_guest_order_links(tenant_id,customer_id,order_id) VALUES(:'t',:'c',:'o');
SET LOCAL ROLE app_runtime;
SELECT app.data_rights_erase_customer(:'c','service',uuidv7());
COMMIT;
SQL
if out=$(apply_forward "$PRIVACY_HARDENING_DB" "${privacy_bounds[0]}" 2>&1); then
 bad 'PRIVACY_UPGRADE accepted ambiguous legacy guest ownership'
elif [[ $out != *PRIVACY_LEGACY_GUEST_REVIEW_REQUIRED* ]]; then
 bad "PRIVACY_UPGRADE failed for another reason: $out"
elif [[ $(db "$PRIVACY_HARDENING_DB" -c "SELECT to_regclass('app.data_rights_erased_orders') IS NULL") != t ]]; then
 bad 'PRIVACY_UPGRADE refusal left partial schema'
else
 pass 'privacy upgrade refuses ambiguous pre-existing erased guest and rolls back'
fi
# The fixture's original ownership is independently known; restore only that
# verified typed mapping, never derive it from the erased email/snapshot.
db "$PRIVACY_HARDENING_DB" <<'SQL' >/dev/null
INSERT INTO app.customer_guest_order_links(tenant_id,customer_id,order_id)
 SELECT e.tenant_id,e.customer_id,o.id FROM app.data_rights_erasures e JOIN app.orders o ON o.tenant_id=e.tenant_id
 WHERE o.creation_key='cd4d4fb9-c7ae-5881-8fe9-fd4aa2422927';
SQL
if ! out=$(apply_forward "$PRIVACY_HARDENING_DB" "${privacy_bounds[0]}" 2>&1); then
 bad "PRIVACY_UPGRADE verified mapping retry failed: $out"
elif [[ $(db "$PRIVACY_HARDENING_DB" -c "SELECT (SELECT count(*) FROM app.data_rights_erased_orders)||'|'||(SELECT count(*) FROM app.customer_guest_order_links)") != '1|0' ]]; then
 bad 'PRIVACY_UPGRADE failed to preserve typed ownership and retire restored proof'
else
 pass 'privacy upgrade accepts independently verified legacy mapping and retires proof'
fi
admin -c "DROP DATABASE $PRIVACY_HARDENING_DB" >/dev/null; PRIVACY_HARDENING_DB=""

# A converter erased under the predecessor schema still owns its immutable
# receipt, but the legacy notification owner omitted quote.converted. Populate
# that history before upgrading; fresh-schema erasure tests cannot expose it.
PRIVACY_HARDENING_DB=privacy_conversion_upgrade_$$
admin -c "CREATE DATABASE $PRIVACY_HARDENING_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${privacy_bounds[0]} ]] || break
 apply_forward "$PRIVACY_HARDENING_DB" "$f"
done
db "$PRIVACY_HARDENING_DB" <<'SQL' >/dev/null
CREATE TABLE public.privacy_conversion_upgrade_witness(
 tenant_id uuid,customer_id uuid,notification_id uuid,erased boolean,
 customer jsonb,delivery jsonb,dispatch jsonb,source jsonb,quote jsonb,receipt jsonb,order_snapshot jsonb
);
DO $fixture$
DECLARE t uuid;c uuid;a uuid;u uuid;org uuid;s uuid;i uuid;co uuid;q uuid;o uuid;e uuid;
 d app.notification_deliveries;tenant_no integer;subject_no integer;contact text;worker uuid:=uuidv7();
BEGIN
 FOR tenant_no IN 1..2 LOOP
  INSERT INTO app.tenants(name) VALUES('privacy-conversion-upgrade-'||tenant_no) RETURNING id INTO t;
  PERFORM set_config('app.tenant_id',t::text,true);
  INSERT INTO app.users(email,password_hash) VALUES(uuidv7()::text||'@staff.invalid','fixture') RETURNING id INTO u;
  INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(t,u,'admin');
  INSERT INTO app.organizations(tenant_id,display_name) VALUES(t,'privacy upgrade') RETURNING id INTO org;
  INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(t,org,'privacy upgrade') RETURNING id INTO s;
  INSERT INTO app.buyer_companies(tenant_id,name) VALUES(t,'privacy upgrade') RETURNING id INTO co;
  INSERT INTO app.customers(tenant_id,email) VALUES(t,'acceptor@example.invalid') RETURNING id INTO a;
  INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code) VALUES(t,co,a,'purchaser');
  FOR subject_no IN 1..3 LOOP
   contact:='legacy-converter-'||tenant_no||'-'||subject_no||'@example.invalid';
   INSERT INTO app.customers(tenant_id,email) VALUES(t,contact) RETURNING id INTO c;
   INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code) VALUES(t,co,c,'approver');
   INSERT INTO app.inquiries(tenant_id,site_id,buyer_email,message) VALUES(t,s,'inquiry@example.invalid','upgrade') RETURNING id INTO i;
   SET LOCAL ROLE app_runtime;
   q:=app.create_quote(t,s,i,co,u,'USD',100,0,0,100,'{}',clock_timestamp()+interval '1 hour',
    '[{"line_no":1,"description":"upgrade","quantity":1,"unit_price":100,"line_total":100,"snapshot":{}}]');
   PERFORM app.transition_quote(t,q,1,'send','staff',u);
   PERFORM app.transition_quote(t,q,2,'accept','customer',a);
   PERFORM app.transition_quote(t,q,3,'approve','customer',c);
   RESET ROLE;
   o:=uuidv7();
   SET LOCAL ROLE app_orders_executor;
   INSERT INTO app.orders(tenant_id,id,customer_id,order_number,creation_key,request_hash,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
    VALUES(t,o,c,subject_no,'00000000-0000-5000-8000-'||lpad(subject_no::text,12,'0'),sha256('conversion'::bytea),'USD',100,0,0,'{}',100);
   SET LOCAL ROLE app_quotes_executor;
   INSERT INTO app.quote_conversions(tenant_id,quote_id,buyer_company_id,customer_id,order_id,quote_version,idempotency_key,request_hash,order_number,currency,total_minor)
    VALUES(t,q,co,c,o,4,'30796ae3-1c13-58bd-8ac3-ee7d23d1f53d',sha256('conversion'::bytea),subject_no,'USD',100);
   RESET ROLE;
   INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
    VALUES(t,'quote.converted',jsonb_build_object('quote_id',q,'order_id',o,'quote_version',4),'quotes','converted:'||q) RETURNING id INTO e;
   SELECT * INTO STRICT d FROM app.notification_deliveries WHERE tenant_id=t AND source_event_id=e;
   IF subject_no<3 THEN
    -- Keep both historical and current dispatch generations. One current
    -- delivery is pending, the other failed, when its owner is erased.
    UPDATE app.notification_deliveries SET status='dead_letter' WHERE tenant_id=t AND id=d.id;
    UPDATE app.outbox_events SET status='dead_letter' WHERE tenant_id=t AND id=d.dispatch_event_id;
    SET LOCAL ROLE app_runtime;
    PERFORM app.notification_redrive(t,d.id,'b402f9bb-95fa-5c2a-aa88-55aeb801e85c');
    PERFORM app.data_rights_erase_customer(c,'service',u);
    RESET ROLE;
    UPDATE app.notification_deliveries SET status=CASE WHEN subject_no=1 THEN 'pending' ELSE 'failed' END WHERE tenant_id=t AND id=d.id;
    SELECT * INTO STRICT d FROM app.notification_deliveries WHERE tenant_id=t AND id=d.id;
    IF d.recipient<>contact OR d.status NOT IN('pending','failed')
     OR NOT EXISTS(SELECT FROM app.data_rights_erasures WHERE tenant_id=t AND customer_id=c) THEN
     RAISE EXCEPTION 'PRIVACY_CONVERSION_UPGRADE predecessor fixture did not retain erased converter email';
    END IF;
    -- Valid lease proves this is a reachable sending bug, not inert history.
    -- Roll back the admission probe so the upgrade sees pending/failed rows.
    BEGIN
     UPDATE app.outbox_events SET status='processing',lease_owner=worker,lease_until=clock_timestamp()+interval '1 hour',attempts=1
      WHERE tenant_id=t AND id=d.dispatch_event_id;
     PERFORM set_config('app.outbox_worker',worker::text,true);
     PERFORM set_config('app.outbox_attempt','1',true);
     SET LOCAL ROLE app_outbox_worker;
     d:=app.notification_begin(t,d.dispatch_event_id,d.id);
     RESET ROLE;
     IF d.status<>'sending' OR d.send_token IS NULL THEN
      RAISE EXCEPTION 'PRIVACY_CONVERSION_UPGRADE predecessor send was not reachable';
     END IF;
     RAISE SQLSTATE 'P0004' USING MESSAGE='rollback admission probe';
    EXCEPTION WHEN assert_failure THEN NULL;
    END;
   END IF;
   INSERT INTO public.privacy_conversion_upgrade_witness
    SELECT t,c,d.id,subject_no<3,to_jsonb(cu),to_jsonb(nd),to_jsonb(de),to_jsonb(se),to_jsonb(qu),to_jsonb(cr),to_jsonb(ord)
    FROM app.customers cu,app.notification_deliveries nd,app.outbox_events de,app.outbox_events se,app.quotes qu,app.quote_conversions cr,app.orders ord
    WHERE cu.tenant_id=t AND cu.id=c AND nd.tenant_id=t AND nd.id=d.id AND de.tenant_id=t AND de.id=nd.dispatch_event_id
     AND se.tenant_id=t AND se.id=e AND qu.tenant_id=t AND qu.id=q AND cr.tenant_id=t AND cr.quote_id=q AND ord.tenant_id=t AND ord.id=o;
  END LOOP;
 END LOOP;
END $fixture$;
SQL
apply_forward "$PRIVACY_HARDENING_DB" "${privacy_bounds[0]}"
if ! out=$(db "$PRIVACY_HARDENING_DB" 2>&1 <<'SQL'
DO $check$
DECLARE w record;d app.notification_deliveries;worker uuid:=uuidv7();claimed uuid[];
BEGIN
 IF (SELECT count(*) FROM public.privacy_conversion_upgrade_witness)<>6 THEN RAISE EXCEPTION 'PRIVACY_CONVERSION_UPGRADE missing witnesses'; END IF;
 FOR w IN SELECT * FROM public.privacy_conversion_upgrade_witness ORDER BY tenant_id,customer_id LOOP
  PERFORM set_config('app.tenant_id',w.tenant_id::text,true);
  SELECT * INTO STRICT d FROM app.notification_deliveries WHERE tenant_id=w.tenant_id AND id=w.notification_id;
  IF w.erased THEN
   IF d.recipient IS DISTINCT FROM 'erased-'||d.id||'@invalid.test' OR d.status<>'dead_letter'
    OR d.last_error IS DISTINCT FROM 'PRIVACY_ERASED' OR d.send_token IS NOT NULL OR d.lease_worker IS NOT NULL OR d.lease_attempt IS NOT NULL THEN
    RAISE EXCEPTION 'PRIVACY_CONVERSION_UPGRADE retained erased converter recipient or send capability';
   END IF;
   IF (SELECT count(*) FROM app.outbox_events WHERE tenant_id=w.tenant_id AND delivery_target='notifications.email'
     AND payload=jsonb_build_object('notification_id',d.id) AND status='dead_letter' AND last_error='PRIVACY_ERASED' AND lease_owner IS NULL AND lease_until IS NULL)<>2 THEN
    RAISE EXCEPTION 'PRIVACY_CONVERSION_UPGRADE did not retire every dispatch generation';
   END IF;
   BEGIN
    SET LOCAL ROLE app_outbox_worker;
    PERFORM app.notification_begin(w.tenant_id,d.dispatch_event_id,d.id);
    RAISE EXCEPTION 'PRIVACY_CONVERSION_UPGRADE admitted retired dispatch';
   EXCEPTION WHEN insufficient_privilege THEN
    IF SQLERRM<>'NOTIFICATIONS_LEASE_FENCE' THEN RAISE; END IF;
   END;
   RESET ROLE;
   BEGIN
    SET LOCAL ROLE app_runtime;
    PERFORM app.notification_redrive(w.tenant_id,d.id,'92920ccc-2983-52fb-a749-4c015f12224c');
    RAISE EXCEPTION 'PRIVACY_CONVERSION_UPGRADE redrove erased notification';
   EXCEPTION WHEN object_not_in_prerequisite_state THEN
    IF SQLERRM<>'PRIVACY_NOTIFICATION_IMMUTABLE' THEN RAISE; END IF;
   END;
   RESET ROLE;
  ELSIF (SELECT to_jsonb(cu) FROM app.customers cu WHERE cu.tenant_id=w.tenant_id AND cu.id=w.customer_id) IS DISTINCT FROM w.customer
   OR to_jsonb(d) IS DISTINCT FROM w.delivery
   OR (SELECT to_jsonb(de) FROM app.outbox_events de WHERE de.tenant_id=w.tenant_id AND de.id=d.dispatch_event_id) IS DISTINCT FROM w.dispatch
   OR (SELECT to_jsonb(se) FROM app.outbox_events se WHERE se.tenant_id=w.tenant_id AND se.id=d.source_event_id) IS DISTINCT FROM w.source
   OR (SELECT to_jsonb(qu) FROM app.quotes qu WHERE qu.tenant_id=w.tenant_id AND qu.id=d.reference_id) IS DISTINCT FROM w.quote THEN
   RAISE EXCEPTION 'PRIVACY_CONVERSION_UPGRADE changed non-erased customer or notification';
  END IF;
  IF (SELECT to_jsonb(cr) FROM app.quote_conversions cr WHERE cr.tenant_id=w.tenant_id AND cr.quote_id=d.reference_id) IS DISTINCT FROM w.receipt
   OR (SELECT to_jsonb(ord) FROM app.orders ord WHERE ord.tenant_id=w.tenant_id AND ord.id=(w.receipt->>'order_id')::uuid) IS DISTINCT FROM w.order_snapshot THEN
   RAISE EXCEPTION 'PRIVACY_CONVERSION_UPGRADE changed immutable conversion or financial evidence';
  END IF;
 END LOOP;
 FOR w IN SELECT DISTINCT tenant_id FROM public.privacy_conversion_upgrade_witness LOOP
  PERFORM set_config('app.tenant_id',w.tenant_id::text,true);
  SET LOCAL ROLE app_outbox_worker;
  SELECT array_agg(e.id) INTO claimed FROM app.claim_notification_outbox(w.tenant_id,worker,1000,60) e;
  RESET ROLE;
  IF EXISTS(SELECT FROM public.privacy_conversion_upgrade_witness v JOIN app.notification_deliveries nd
    ON nd.tenant_id=v.tenant_id AND nd.id=v.notification_id WHERE v.tenant_id=w.tenant_id AND v.erased AND nd.dispatch_event_id=ANY(claimed)) THEN
   RAISE EXCEPTION 'PRIVACY_CONVERSION_UPGRADE claimed erased notification';
  END IF;
  IF NOT EXISTS(SELECT FROM public.privacy_conversion_upgrade_witness v WHERE v.tenant_id=w.tenant_id AND NOT v.erased
   AND (v.delivery->>'dispatch_event_id')::uuid=ANY(claimed)) THEN
   RAISE EXCEPTION 'PRIVACY_CONVERSION_UPGRADE non-erased notification no longer dispatchable';
  END IF;
 END LOOP;
END $check$;
SQL
); then
 bad "privacy legacy conversion populated upgrade: $out"
else
 pass 'privacy upgrade scrubs already-erased converters, retires all generations, rejects dispatch/redrive and preserves unrelated/financial rows'
fi
admin -c "DROP DATABASE $PRIVACY_HARDENING_DB" >/dev/null; PRIVACY_HARDENING_DB=""

# B369: preserve an existing manual audit verbatim while replacing its identity.
# Discover the integration-assigned number and apply every predecessor, including
# other modules; never compare numeric strings with leading zeroes.
audit_files=(migrations/[0-9][0-9][0-9]_refund_resolution_identity.sql)
[[ ${#audit_files[@]} == 1 ]] || { bad 'exactly one refund audit identity migration required'; exit 1; }
AUDIT_DB=upgrade_refund_audit_$$
admin -c "CREATE DATABASE $AUDIT_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${audit_files[0]} ]] || break
 apply_forward "$AUDIT_DB" "$f"
done
db "$AUDIT_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('audit-upgrade') RETURNING id AS t \gset
SELECT set_config('app.tenant_id',:'t',false);
INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment)
 VALUES(:'t','fake','acct_audit_upgrade','test');
INSERT INTO app.payment_refund_cases(tenant_id,provider,provider_account_id,case_key,provider_refund_id,provider_ref,reason,evidence_hash)
 VALUES(:'t','fake','acct_audit_upgrade','refund:upgrade','upgrade','capture','unmatched_refund',sha256('original'::bytea)) RETURNING id AS c \gset
INSERT INTO app.payment_refund_case_resolutions(tenant_id,case_id,outcome,reason,verifier,evidence_hash)
 VALUES(:'t',:'c','resolved_no_posting','no_posting_required',uuidv7(),sha256('staff'::bytea));
SQL
before_audit=$(db "$AUDIT_DB" -c "SELECT row_to_json(r)::text FROM app.payment_refund_case_resolutions r;")
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${audit_files[0]} ]] && continue
 apply_forward "$AUDIT_DB" "$f"
done
after_audit=$(db "$AUDIT_DB" -c "SELECT row_to_json(r)::text FROM app.payment_refund_case_resolutions r;")
[[ -n $before_audit && $before_audit == "$after_audit" ]] || bad 'B369_UPGRADE_REWROTE_AUDIT'
db "$AUDIT_DB" -f tests/db_role_matrix.sql >/dev/null
db "$AUDIT_DB" -f tests/db_refund_recovery.sql >/dev/null
admin -c "DROP DATABASE $AUDIT_DB" >/dev/null; AUDIT_DB=""
pass 'B369 upgrade preserves manual verifier, evidence, outcome and DB timestamp verbatim'


# MDEL-02: populated global identities, memberships, keys and lifecycle metadata
# survive the complete forward upgrade byte-for-byte. Discover by stable suffix.
identity_merchant_files=(migrations/[0-9][0-9][0-9]_identity_merchant_deletion.sql)
[[ ${#identity_merchant_files[@]} == 1 ]] || { bad 'exactly one Identity merchant migration required'; exit 1; }
IDENTITY_MERCHANT_DB=upgrade_identity_merchant_$$
admin -c "CREATE DATABASE $IDENTITY_MERCHANT_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f == "${identity_merchant_files[0]}" ]] && break
 apply_forward "$IDENTITY_MERCHANT_DB" "$f"
done
db "$IDENTITY_MERCHANT_DB" <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('mdel02-upgrade-a'),('mdel02-upgrade-b');
INSERT INTO app.users(email,password_hash) VALUES('mdel02-upgrade@invalid.test','upgrade-password') RETURNING id AS u \gset
INSERT INTO app.tenant_memberships(tenant_id,user_id,role) SELECT id,:'u','owner' FROM app.tenants;
INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES(:'u',sha256('upgrade-session'::bytea),clock_timestamp()+interval '1 day');
INSERT INTO app.api_keys(tenant_id,issued_by,key_prefix,key_hash) SELECT id,:'u','upgrade',sha256(id::text::bytea) FROM app.tenants;
CREATE TABLE public.mdel02_before AS
 SELECT 'users' AS tab,to_jsonb(x) AS snapshot FROM app.users x UNION ALL
 SELECT 'sessions',to_jsonb(x) FROM app.auth_sessions x UNION ALL
 SELECT 'memberships',to_jsonb(x) FROM app.tenant_memberships x UNION ALL
 SELECT 'keys',to_jsonb(x) FROM app.api_keys x UNION ALL
 SELECT 'lifecycle',to_jsonb(x) FROM app.tenant_lifecycles x;
SQL
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${identity_merchant_files[0]} ]] && continue
 apply_forward "$IDENTITY_MERCHANT_DB" "$f"
done
db "$IDENTITY_MERCHANT_DB" <<'SQL' >/dev/null
DO $check$
BEGIN
 IF EXISTS(WITH actual AS(
  SELECT 'users' AS tab,to_jsonb(x) AS snapshot FROM app.users x UNION ALL
  SELECT 'sessions',to_jsonb(x) FROM app.auth_sessions x UNION ALL
  SELECT 'memberships',to_jsonb(x) FROM app.tenant_memberships x UNION ALL
  SELECT 'keys',to_jsonb(x) FROM app.api_keys x UNION ALL
  SELECT 'lifecycle',to_jsonb(x) FROM app.tenant_lifecycles x)
  SELECT FROM ((SELECT * FROM actual EXCEPT SELECT * FROM public.mdel02_before)
   UNION ALL (SELECT * FROM public.mdel02_before EXCEPT SELECT * FROM actual)) delta) THEN
  RAISE EXCEPTION 'MDEL02_UPGRADE_CHANGED_GLOBAL_AUTHORITY'; END IF;
END $check$;
SQL
db "$IDENTITY_MERCHANT_DB" -f tests/db_identity_merchant_deletion.sql >/dev/null
db "$IDENTITY_MERCHANT_DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $IDENTITY_MERCHANT_DB" >/dev/null; IDENTITY_MERCHANT_DB=""
pass 'MDEL02 populated forward upgrade preserves global identities and all existing tenant authority'

# Aggregate only after all module upgrade steps.
[[ $fail -eq 0 ]] || exit 1
echo 'PASS: security, reliability, inventory/holder authority, pricing, shipping, tax, customers/B2B, ledger, catalog, orders, queue, reliability limits, cart, storefront serving, ops metrics, notifications, quotes, identity auth/login, CMS, checkout, customer auth payments and fulfillment/returns upgrade gates'
