#!/usr/bin/env bash
# Upgrade gates for migration 010 on data written under 001-009; real PG18 only.
# Depends on: a superuser DATABASE_URL (or Docker compose) able to CREATE DATABASE.
# Called by: CI after check_pg.sh. Each case runs in its own throwaway database.
# Invariants: 010 refuses to seal a live release with unsafe media; every pre-010
# release ends up sealed (history is immutable); releases created after 010 are not.
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

[[ $fail -eq 0 ]] || exit 1
echo 'PASS: 010 upgrade gates'
