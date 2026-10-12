#!/usr/bin/env bash
# Upgrade gates for security hardening, forward reliability, additive inventory and identity; real PG18 only.
# Depends on: a superuser DATABASE_URL (or Docker compose) able to CREATE DATABASE.
# Called by: CI after check_pg.sh. Each case runs in its own throwaway database.
# Invariants: 010 refuses to seal a live release with unsafe media; every pre-010
# release ends up sealed (history is immutable); releases created after 010 are not.
# Reliability preserves compatible pending data and refuses ambiguous old results.
# Identity preserves populated global credentials and tenant-bound membership/key state.
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
IDENTITY_DB=""
LOGIN_DB=""
trap '[[ -z $RELIABILITY_DB ]] || admin -c "DROP DATABASE IF EXISTS $RELIABILITY_DB" >/dev/null 2>&1; [[ -z $SHIPPING_DB ]] || admin -c "DROP DATABASE IF EXISTS $SHIPPING_DB" >/dev/null 2>&1; [[ -z $IDENTITY_DB ]] || admin -c "DROP DATABASE IF EXISTS $IDENTITY_DB" >/dev/null 2>&1; [[ -z $LOGIN_DB ]] || admin -c "DROP DATABASE IF EXISTS $LOGIN_DB" >/dev/null 2>&1' EXIT
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
 { echo 'BEGIN;'; cat "$f"; echo 'COMMIT;'; } | db "$DB" >/dev/null
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
{ echo 'BEGIN;'; cat "${identity_files[0]}"; echo 'COMMIT;'; } | db "$DB" >/dev/null
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
 { echo 'BEGIN;'; cat "$f"; echo 'COMMIT;'; } | db "$DB" >/dev/null
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
 { echo 'BEGIN;'; cat "$f"; echo 'COMMIT;'; } | db "$DB" >/dev/null
done
db "$DB" <<'SQL' >/dev/null
INSERT INTO app.users(email,password_hash) VALUES('login-upgrade@example.test','fixture-only') RETURNING id AS u \gset
INSERT INTO app.auth_sessions(user_id,token_hash,created_at,expires_at,revoked_at) VALUES
 (:'u',sha256('legacy-live'::bytea),'2040-01-01Z','2040-01-02Z',NULL),
 (:'u',sha256('legacy-expired'::bytea),'2020-01-01Z','2020-01-02Z',NULL),
 (:'u',sha256('legacy-revoked'::bytea),'2040-01-01Z','2040-01-02Z','2040-01-01T00:01Z');
CREATE TABLE public.login_upgrade_expected AS SELECT id,to_jsonb(s) AS payload FROM app.auth_sessions s;
SQL
{ echo 'BEGIN;'; cat "${login_files[0]}"; echo 'COMMIT;'; } | db "$DB" >/dev/null
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
 { echo 'BEGIN;'; cat "$f"; echo 'COMMIT;'; } | db "$DB" >/dev/null
done
db "$DB" -f tests/db_identity_login.sql >/dev/null
db "$DB" -f tests/db_identity_auth.sql >/dev/null
db "$DB" -f tests/db_role_matrix.sql >/dev/null
admin -c "DROP DATABASE $DB"
LOGIN_DB=""

[[ $fail -eq 0 ]] || exit 1
echo 'PASS: security, reliability, inventory, pricing, shipping and identity upgrade gates'
