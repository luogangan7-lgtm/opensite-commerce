#!/usr/bin/env bash
# Multi-connection gates for release sealing and pricing tier uniqueness; real PG18.
# Depends on: an expendable DB already migrated by check_pg.sh; psql or Docker compose.
# Called by: CI after check_pg.sh. Invariant checked: a sealed release never
# references media that is non-public or whose blob is not ready, whatever the
# interleaving of runtime child insert, media downgrade and publish.
# Pricing also rejects two concurrent inserts for the same list/SKU/quantity tier.
# Ordering is explicit, not timed: a participant that must hold its locks parks
# on an advisory lock owned by a driver "gate" session, and the driver polls
# pg_stat_activity (superuser) until each participant is parked or blocked on
# the expected lock before releasing the next step.
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

# tx <label> <role> <tenant> <sql>: one transaction on its own connection, named
# race_<label> for the driver. Never aborts the gate; stderr is kept per label.
ERRDIR=$(mktemp -d)
cleanup() {  # never leave parked participants or gate sessions behind
  sql <<<"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name LIKE 'race\_%';" >/dev/null 2>&1 || true
  rm -rf "$ERRDIR"
}
trap cleanup EXIT
tx() {
  printf "SET application_name='race_%s'; BEGIN; SELECT set_config('app.tenant_id','%s',true); SET LOCAL ROLE %s; %s COMMIT;\n" \
    "$1" "$3" "$2" "$4" | sql >/dev/null 2>"$ERRDIR/$1" || true
}
park() { echo "SELECT pg_advisory_xact_lock_shared($1);"; }   # SQL: wait for gate $1
gate_close() {  # driver session holds advisory lock $1 until gate_open
  echo "SET application_name='race_gate_$1'; SELECT pg_advisory_lock($1); SELECT pg_sleep(600);" | sql >/dev/null 2>&1 &
  await "gate_$1" ClientRead PgSleep
}
gate_open() { sql <<<"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name='race_gate_$1';" >/dev/null; }
# await <label> <wait_event...|gone>: poll until race_<label> waits on one of the
# events (or has disconnected, for 'gone'); 15s timeout fails the gate loudly.
await() {
  local label=$1; shift; local i ev w
  for i in $(seq 1 300); do
    ev=$(sql <<<"SELECT coalesce(max(wait_event),'none') FROM pg_stat_activity WHERE application_name='race_$label';
                 SELECT count(*) FROM pg_stat_activity WHERE application_name='race_$label';" | paste -sd' ' -)
    for w in "$@"; do
      [[ $w == gone && ${ev#* } == 0 ]] && return 0
      [[ ${ev%% *} == "$w" ]] && return 0
    done
    sleep 0.05
  done
  echo "  sync timeout: race_$label never reached ${*} (last: $ev)"; fail=1; return 1
}
ROWLOCK=(transactionid tuple)

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
check() {  # exact end state, so a broken fixture/grant/ordering cannot pass silently
  local got; got=$(witness "$3" "$4" "$5")
  echo "$1: $got (expected $2)"
  [[ $got == "$2" ]] || fail=1
}

fail=0
# Child appended but uncommitted -> media downgrades (cannot see the child) ->
# child commits -> publisher seals (must wait for the downgrade's row lock) ->
# downgrade commits -> seal refuses (23514).
for target in asset blob; do
  read -r T S R B A <<<"$(fixture)"
  if [[ $target == asset ]]; then down="UPDATE app.media_assets SET visibility='private' WHERE id='$A';"; want='false|private|ready'
  else down="UPDATE app.media_blobs SET status='deleting' WHERE id='$B';"; want='false|public|deleting'; fi
  gate_close 1; gate_close 2
  tx child app_runtime "$T" "INSERT INTO app.site_release_media(tenant_id,site_id,release_id,media_id) VALUES('$T','$S','$R','$A'); $(park 1)" &
  await child advisory
  tx down app_media_service "$T" "$down $(park 2)" &
  await down advisory
  gate_open 1; await child gone
  tx seal app_publisher "$T" "SELECT app.publish_release('$T','$S','$R',NULL);" &
  await seal "${ROWLOCK[@]}"
  gate_open 2; wait
  check "race child-uncommitted / $target" "$want" "$R" "$A" "$B"
  expect child ok; expect down ok; expect seal RELEASE_MEDIA_NOT_PUBLIC_READY
done
# Committed child; downgrade and seal race in both orders: the first holds its
# locks (parked) until the second is observed blocked on them, so it must lose.
for first in media seal; do
  read -r T S R B A <<<"$(fixture)"
  tx child app_runtime "$T" "INSERT INTO app.site_release_media(tenant_id,site_id,release_id,media_id) VALUES('$T','$S','$R','$A');"
  down="UPDATE app.media_blobs SET status='deleting' WHERE id='$B';"
  seal="SELECT app.publish_release('$T','$S','$R',NULL);"
  if [[ $first == media ]]; then a=(down app_media_service "$down"); b=(seal app_publisher "$seal"); want='false|public|deleting'
  else a=(seal app_publisher "$seal"); b=(down app_media_service "$down"); want='true|public|ready'; fi
  gate_close 3
  tx "${a[0]}" "${a[1]}" "$T" "${a[2]} $(park 3)" &
  await "${a[0]}" advisory
  tx "${b[0]}" "${b[1]}" "$T" "${b[2]}" &
  await "${b[0]}" "${ROWLOCK[@]}"
  gate_open 3; wait
  check "race committed-child / $first first" "$want" "$R" "$A" "$B"
  expect child ok
  if [[ $first == media ]]; then expect down ok; expect seal RELEASE_MEDIA_NOT_PUBLIC_READY
  else expect seal ok; expect down SEALED_RELEASE_MEDIA_PINNED; fi
done
# Pricing's unique tier must survive two trusted writers on separate connections.
# No numbered migration is assumed; this is an invariant of the pricing tables.
read -r T L K <<<"$(sql <<'SQL'
INSERT INTO app.tenants(name) VALUES('pricing-race') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','R') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 VALUES(:'t',:'p','R','default') RETURNING id AS k \gset
INSERT INTO app.price_lists(tenant_id,currency,status,starts_at)
 VALUES(:'t','USD','active',now()) RETURNING id AS l \gset
SELECT :'t'||' '||:'l'||' '||:'k';
SQL
)"
insert="INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount) VALUES('$T','$L','$K',1,100);"
gate_close 10
tx pricing_first postgres "$T" "$insert $(park 10)" &
await pricing_first advisory
tx pricing_second postgres "$T" "$insert" &
await pricing_second "${ROWLOCK[@]}"
gate_open 10; wait
expect pricing_first ok; expect pricing_second price_entries_tier_unique
got=$(sql <<<"SELECT count(*) FROM app.price_entries WHERE tenant_id='$T' AND price_list_id='$L' AND sku_id='$K' AND min_quantity=1;")
[[ $got == 1 ]] || { echo "FAIL: pricing race persisted $got duplicate tiers"; fail=1; }
echo "pricing duplicate-tier race: $got committed tier (expected 1)"
if [[ $fail -ne 0 ]]; then echo "FAIL: race end state differs from the sealing/pricing contract" >&2; exit 1; fi
echo 'PASS: lock-synchronized sealing and pricing races preserve media and tier uniqueness'
