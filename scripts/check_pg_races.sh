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

# tx <label> <role> <tenant> <sql>: one transaction on its own connection. Never
# aborts the gate; stderr is kept per label so expect() can check who failed why.
ERRDIR=$(mktemp -d); trap 'rm -rf "$ERRDIR"' EXIT
tx() {
  printf "BEGIN; SELECT set_config('app.tenant_id','%s',true); SET LOCAL ROLE %s; %s COMMIT;\n" "$3" "$2" "$4" \
    | sql >/dev/null 2>"$ERRDIR/$1" || true
}
# expect <label> <marker|ok>: winners must not error; losers only with their guard.
expect() {
  local err; err=$(cat "$ERRDIR/$1")
  if [[ $2 == ok ]]; then [[ -z $err ]] && return; else [[ $err == *"$2"* ]] && return; fi
  echo "  $1: expected ${2}, got: ${err:-no error}"; fail=1
}

witness() {  # sealed|asset visibility|blob status for release $1 and its fixture media
  sql <<SQL
SELECT r.sealed||'|'||a.visibility||'|'||b.status FROM app.site_releases r, app.media_assets a, app.media_blobs b
 WHERE r.id='$1' AND a.id='$2' AND b.id='$3';
SQL
}

# check <label> <expected> <R> <A> <B>: exact end state, so a broken fixture,
# grant or timing cannot pass silently (a missing row prints nothing -> fail).
check() {
  local got; got=$(witness "$3" "$4" "$5")
  echo "$1: $got (expected $2)"
  [[ $got == "$2" ]] || fail=1
}

fail=0
# Each case: runtime appends the child (uncommitted) -> media downgrades (cannot see
# the child) -> runtime commits -> publisher seals -> media commits.
for downgrade in "UPDATE app.media_assets SET visibility='private' WHERE id='%a';" \
                 "UPDATE app.media_blobs SET status='deleting' WHERE id='%b';"; do
  read -r T S R B A <<<"$(fixture)"
  down=${downgrade//%a/$A}; down=${down//%b/$B}
  tx child app_runtime "$T" "INSERT INTO app.site_release_media(tenant_id,site_id,release_id,media_id) VALUES('$T','$S','$R','$A'); SELECT pg_sleep(1.5);" &
  sleep 0.5
  tx down app_media_service "$T" "$down SELECT pg_sleep(2.5);" &
  sleep 1.5
  tx seal app_publisher "$T" "SELECT app.publish_release('$T','$S','$R',NULL);" &
  wait
  # Seal must wait for the downgrade, then refuse (23514): unsealed, downgrade kept.
  [[ $down == *media_assets* ]] && want='false|private|ready' || want='false|public|deleting'
  check "race child-uncommitted / ${down%% SET*}" "$want" "$R" "$A" "$B"
  expect child ok; expect down ok; expect seal RELEASE_MEDIA_NOT_PUBLIC_READY
done
# Committed child; downgrade and seal race in both orders (the second must lose).
for first in media seal; do
  read -r T S R B A <<<"$(fixture)"
  tx child app_runtime "$T" "INSERT INTO app.site_release_media(tenant_id,site_id,release_id,media_id) VALUES('$T','$S','$R','$A');"
  down="UPDATE app.media_blobs SET status='deleting' WHERE id='$B'; SELECT pg_sleep(1.5);"
  seal="SELECT app.publish_release('$T','$S','$R',NULL); SELECT pg_sleep(1.5);"
  if [[ $first == media ]]; then a=(down app_media_service "$down"); b=(seal app_publisher "$seal")
  else a=(seal app_publisher "$seal"); b=(down app_media_service "$down"); fi
  tx "${a[0]}" "${a[1]}" "$T" "${a[2]}" & sleep 0.5
  tx "${b[0]}" "${b[1]}" "$T" "${b[2]}" & wait
  # The second writer loses: media-first -> seal refused; seal-first -> downgrade refused.
  [[ $first == media ]] && want='false|public|deleting' || want='true|public|ready'
  check "race committed-child / $first first" "$want" "$R" "$A" "$B"
  expect child ok
  if [[ $first == media ]]; then expect down ok; expect seal RELEASE_MEDIA_NOT_PUBLIC_READY
  else expect seal ok; expect down SEALED_RELEASE_MEDIA_PINNED; fi
done
if [[ $fail -ne 0 ]]; then echo "FAIL: race end state differs from the sealing contract" >&2; exit 1; fi
echo 'PASS: 3-connection seal/downgrade/child-append races keep sealed media public+ready'
