#!/usr/bin/env bash
# Upgrade gates for security hardening, forward reliability and additive inventory; real PG18 only.
# Depends on: a superuser DATABASE_URL (or Docker compose) able to CREATE DATABASE.
# Called by: CI after check_pg.sh. Each case runs in its own throwaway database.
# Invariants: 010 refuses to seal a live release with unsafe media; every pre-010
# release ends up sealed (history is immutable); releases created after 010 are not.
# Reliability preserves compatible pending data and refuses ambiguous old results.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
if [[ -n "${DATABASE_URL:-}" ]]; then
  base="${DATABASE_URL%%\?*}"; query=""; [[ $DATABASE_URL == *\?* ]] && query="?${DATABASE_URL#*\?}"
  admin() { psql "$DATABASE_URL" -X -q -At -v ON_ERROR_STOP=1 "$@"; }
  db() { local name=$1; shift; psql "${base%/*}/$name$query" -X -q -At -v ON_ERROR_STOP=1 "$@"; }
else
  admin() { docker compose -f deploy/compose.yaml exec -T postgres psql -X -q -At -U postgres -d opensite -v ON_ERROR_STOP=1 "$@"; }
  db() { local name=$1; shift; docker compose -f deploy/compose.yaml exec -T postgres psql -X -q -At -U postgres -d "$name" -v ON_ERROR_STOP=1 "$@"; }
fi

fresh_009() {  # $1 = database name; applies 001-009 exactly as shipped
  admin -c "DROP DATABASE IF EXISTS $1" -c "CREATE DATABASE $1" 2>/dev/null
  for f in migrations/00[1-9]_*.sql; do db "$1" < "$f" >/dev/null; done
}
apply_010() { { echo 'BEGIN;'; cat migrations/010_security_hardening.sql; echo 'COMMIT;'; } | db "$1" 2>&1 >/dev/null; }

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
  { echo 'BEGIN;'; cat "$f"; echo 'COMMIT;'; } | db "$1" >/dev/null
 done
}
apply_reliability() { { echo 'BEGIN;'; cat "${reliability[0]}"; echo 'COMMIT;'; } | db "$1" 2>&1 >/dev/null; }
RELIABILITY_DB=""
SHIPPING_DB=""
TAX_DB=""
CUSTOMERS_DB=""
CART_DB=""
cleanup_forward_databases() {
 # Attempt each owned database independently even when an earlier DROP fails.
 local name
 for name in "$RELIABILITY_DB" "$SHIPPING_DB" "$TAX_DB" "$CUSTOMERS_DB" "$CART_DB"; do
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
 VALUES(:'t','service',uuidv7(),'legacy','one',sha256('request'::bytea),now()+interval '1 day');
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
  db "$RELIABILITY_DB" -c "INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,state,response_status,response_body) SELECT id,'service',uuidv7(),'legacy','one',sha256('request'::bytea),now()+interval '1 day','$legacy_state',201,'{\"legacy\":true}' FROM app.tenants" >/dev/null
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
DB=upgrade_inventory_$$
fresh_009 $DB
# Every forward migration that precedes inventory (010, 011, ...), in order.
for f in migrations/0[1-9][0-9]_*.sql; do
  [[ $f == *_inventory.sql ]] && continue  # the glob already excludes 001-009
  { echo 'BEGIN;'; cat "$f"; echo 'COMMIT;'; } | db $DB >/dev/null
done
# No inventory tables existed when this tenant and SKU were written.
db $DB <<'SQL' >/dev/null
INSERT INTO app.tenants(name) VALUES('inventory-upgrade') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','existing-product') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 VALUES(:'t',:'p','existing-sku','default');
SQL
inventory_files=(migrations/*_inventory.sql)
[[ ${#inventory_files[@]} -eq 1 && -f ${inventory_files[0]} ]] || { echo 'FAIL: exactly one inventory migration required'; exit 1; }
{ echo 'BEGIN;'; cat "${inventory_files[0]}"; echo 'COMMIT;'; } | db $DB >/dev/null
result=$(db $DB <<'SQL'
SELECT set_config('app.tenant_id',(SELECT id::text FROM app.tenants WHERE name='inventory-upgrade'),false);
SELECT set_config('upgrade.sku',(SELECT id::text FROM app.product_variants WHERE sku='existing-sku'),false);
SET ROLE app_inventory_manager;
SELECT app.inventory_adjust(current_setting('upgrade.sku')::uuid,2,'existing SKU receipt',uuidv7(),'inbound');
RESET ROLE;
SET ROLE app_runtime;
SELECT (app.inventory_reserve(current_setting('upgrade.sku')::uuid,1,'checkout',uuidv7(),'upgrade-key',900)).id AS rid \gset
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
# Full scoped SQL and exact authority matrix also run against the upgraded DB.
db $DB -f tests/db_inventory.sql >/dev/null
db $DB -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $DB"

# Existing catalog rows from 001..010 remain usable by the forward pricing DDL.
# Apply every post-010 migration by its current filename, not a reserved number.
DB=upgrade_pricing_$$
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
  if ((10#$number>10)); then db "$DB" < "$f" >/dev/null; fi
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
 { echo 'BEGIN;'; cat "$f"; echo 'COMMIT;'; } | db "$DB" >/dev/null
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
 { echo 'BEGIN;'; cat "$f"; echo 'COMMIT;'; } | db "$DB" >/dev/null
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
 { echo 'BEGIN;'; cat "$f"; echo 'COMMIT;'; } | db "$TAX_DB" >/dev/null
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
 { echo 'BEGIN;'; cat "$f"; echo 'COMMIT;'; } | db "$TAX_DB" >/dev/null
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
  { echo 'BEGIN;'; cat "$f"; echo 'COMMIT;'; } | db "$1" >/dev/null
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
  { echo 'BEGIN;'; cat "$f"; echo 'COMMIT;'; } | db "$CUSTOMERS_DB" >/dev/null
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
# Cart additive upgrade: all predecessors, populated customers/catalog, then
# the logical *_cart suffix. Number reservations can be reconciled without drift.
cart=(migrations/[0-9][0-9][0-9]_cart.sql)
[[ ${#cart[@]} == 1 && -f ${cart[0]} ]] || { echo 'FAIL: exactly one cart migration required' >&2; exit 1; }
CART_DB=cart_upgrade_$$
admin -c "CREATE DATABASE $CART_DB" >/dev/null
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 [[ $f < ${cart[0]} ]] || break
 { echo 'BEGIN;'; cat "$f"; echo 'COMMIT;'; } | db "$CART_DB" >/dev/null
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
 { echo 'BEGIN;'; cat "$f"; echo 'COMMIT;'; } | db "$CART_DB" >/dev/null
done
if ! db "$CART_DB" < tests/db_cart.sql >/dev/null || ! db "$CART_DB" < tests/db_role_matrix.sql >/dev/null; then
 bad 'cart owner/constraints/role matrix after populated upgrade'
else pass 'cart upgrade preserves prior customers/catalog; owner/CAS and exact privilege gate pass'; fi
admin -c "DROP DATABASE $CART_DB" >/dev/null; CART_DB=""

[[ $fail -eq 0 ]] || exit 1
echo 'PASS: security, reliability, inventory, pricing, shipping, tax, customers/B2B and cart upgrade gates'
