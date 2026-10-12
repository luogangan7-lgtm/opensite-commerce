#!/usr/bin/env bash
# Multi-connection race gates for release sealing (migration 010); real PG18 only.
# Depends on: an expendable DB already migrated by check_pg.sh; psql or Docker compose.
# Called by: CI after check_pg.sh. Invariant checked: a sealed release never
# references media that is non-public or whose blob is not ready, whatever the
# interleaving of runtime child insert, media downgrade and publish.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
if [[ -n "${DATABASE_URL:-}" ]]; then
  PSQL=(psql "$DATABASE_URL" -X -q -At -v ON_ERROR_STOP=1)
else
  PSQL=(docker compose -f deploy/compose.yaml exec -T postgres psql -X -q -At -U postgres -d opensite -v ON_ERROR_STOP=1)
fi
sql() { "${PSQL[@]}"; }

fixture() {
  sql <<'SQL' | tail -1
DO $f$ DECLARE t uuid; o uuid; s uuid; r uuid; b uuid; a uuid; BEGIN
 INSERT INTO app.tenants(name) VALUES('race') RETURNING id INTO t;
 INSERT INTO app.organizations(tenant_id,display_name) VALUES(t,'R') RETURNING id INTO o;
 INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(t,o,'R') RETURNING id INTO s;
 INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES(t,s,1,'race','{}',sha256(uuidv7()::text::bytea)) RETURNING id INTO r;
 INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes,status)
 VALUES(t,'local',uuidv7()::text,sha256(uuidv7()::text::bytea),1,'ready') RETURNING id INTO b;
 INSERT INTO app.media_assets(tenant_id,blob_id,filename,mime_type,visibility)
 VALUES(t,b,'race','image/png','public') RETURNING id INTO a;
 PERFORM set_config('race.ids',concat_ws(' ',t,s,r,b,a),false);
END $f$;
SELECT current_setting('race.ids');
SQL
}

# tx <role> <tenant> <sql>: one transaction on its own connection; never aborts the gate.
tx() {
  printf "BEGIN; SELECT set_config('app.tenant_id','%s',true); SET LOCAL ROLE %s; %s COMMIT;\n" "$2" "$1" "$3" \
    | sql >/dev/null 2>&1 || true
}

witness() {  # prints 'unsafe' if a sealed release references non-public/non-ready media
  sql <<SQL
SELECT CASE WHEN EXISTS (SELECT FROM app.site_releases r
 JOIN app.site_release_media rm ON (rm.tenant_id,rm.site_id,rm.release_id)=(r.tenant_id,r.site_id,r.id)
 JOIN app.media_assets a ON (a.tenant_id,a.id)=(rm.tenant_id,rm.media_id)
 JOIN app.media_blobs b ON (b.tenant_id,b.id)=(a.tenant_id,a.blob_id)
 WHERE r.id='$1' AND r.sealed AND (a.visibility<>'public' OR b.status<>'ready'))
 THEN 'unsafe' ELSE 'safe' END;
SQL
}

fail=0
# Each case: runtime appends the child (uncommitted) -> media downgrades (cannot see
# the child) -> runtime commits -> publisher seals -> media commits.
for downgrade in "UPDATE app.media_assets SET visibility='private' WHERE id='%a';" \
                 "UPDATE app.media_blobs SET status='deleting' WHERE id='%b';"; do
  read -r T S R B A <<<"$(fixture)"
  down=${downgrade//%a/$A}; down=${down//%b/$B}
  tx app_runtime "$T" "INSERT INTO app.site_release_media(tenant_id,site_id,release_id,media_id) VALUES('$T','$S','$R','$A'); SELECT pg_sleep(1.5);" &
  sleep 0.5
  tx app_media_service "$T" "$down SELECT pg_sleep(2.5);" &
  sleep 1.5
  tx app_publisher "$T" "SELECT app.publish_release('$T','$S','$R',NULL);" &
  wait
  result=$(witness "$R")
  echo "race child-uncommitted / ${down%% SET*}: $result"
  [[ $result == safe ]] || fail=1
done
# Committed child; downgrade and seal race in both orders (the second must lose).
for first in media seal; do
  read -r T S R B A <<<"$(fixture)"
  tx app_runtime "$T" "INSERT INTO app.site_release_media(tenant_id,site_id,release_id,media_id) VALUES('$T','$S','$R','$A');"
  down="UPDATE app.media_blobs SET status='deleting' WHERE id='$B'; SELECT pg_sleep(1.5);"
  seal="SELECT app.publish_release('$T','$S','$R',NULL); SELECT pg_sleep(1.5);"
  if [[ $first == media ]]; then a=(app_media_service "$down"); b=(app_publisher "$seal")
  else a=(app_publisher "$seal"); b=(app_media_service "$down"); fi
  tx "${a[0]}" "$T" "${a[1]}" & sleep 0.5
  tx "${b[0]}" "$T" "${b[1]}" & wait
  result=$(witness "$R")
  echo "race committed-child / $first first: $result"
  [[ $result == safe ]] || fail=1
done
if [[ $fail -ne 0 ]]; then echo "FAIL: sealed release references unsafe media" >&2; exit 1; fi
echo 'PASS: 3-connection seal/downgrade/child-append races keep sealed media public+ready'
