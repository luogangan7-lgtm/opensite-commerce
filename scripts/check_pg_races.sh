#!/usr/bin/env bash
# Multi-connection release and inventory race gates; real PG18 only.
# Depends on: an expendable DB already migrated by check_pg.sh; psql or Docker compose.
# Called by: CI after check_pg.sh. Invariant checked: a sealed release never
# references media that is non-public or whose blob is not ready, whatever the
# interleaving of runtime child insert, media downgrade and publish.
# Ordering is explicit, not timed: a participant that must hold its locks parks
# on an advisory lock owned by a driver "gate" session, and the driver polls
# pg_stat_activity (superuser) until each participant is parked or blocked on
# the expected lock before releasing the next step.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
if [[ -n "${DATABASE_URL:-}" ]]; then
  PSQL=(psql "$DATABASE_URL" -X -q -At -v ON_ERROR_STOP=1 -v VERBOSITY=verbose)
else
  PSQL=(docker compose -f deploy/compose.yaml exec -T postgres psql -X -q -At -U postgres -d opensite -v ON_ERROR_STOP=1 -v VERBOSITY=verbose)
fi
sql() { "${PSQL[@]}" "$@"; }

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
RACE_PREFIX="race_${BASHPID}_"
cleanup() {  # never leave parked participants or gate sessions behind
  sql <<<"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=current_database() AND left(application_name,length('$RACE_PREFIX'))='$RACE_PREFIX';" >/dev/null 2>&1 || true
  rm -rf "$ERRDIR"
}
trap cleanup EXIT
tx() {
  printf "SET application_name='%s%s'; BEGIN; SELECT set_config('app.tenant_id','%s',true); SET LOCAL ROLE %s; %s COMMIT;\n" \
    "$RACE_PREFIX" "$1" "$3" "$2" "$4" | sql >/dev/null 2>"$ERRDIR/$1" || true
}
park() { echo "SELECT pg_advisory_xact_lock_shared($1);"; }   # SQL: wait for gate $1
gate_close() {  # driver session holds advisory lock $1 until gate_open
  echo "SET application_name='${RACE_PREFIX}gate_$1'; SELECT pg_advisory_lock($1); SELECT pg_sleep(600);" | sql >/dev/null 2>&1 &
  await "gate_$1" ClientRead PgSleep
}
gate_open() { sql <<<"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name='${RACE_PREFIX}gate_$1';" >/dev/null; }
# await <label> <wait_event...|gone>: poll until race_<label> waits on one of the
# events (or has disconnected, for 'gone'); 15s timeout fails the gate loudly.
await() {
  local label=$1; shift; local i ev w
  for i in $(seq 1 300); do
    ev=$(sql <<<"SELECT coalesce(max(wait_event),'none') FROM pg_stat_activity WHERE application_name='${RACE_PREFIX}$label';
                 SELECT count(*) FROM pg_stat_activity WHERE application_name='${RACE_PREFIX}$label';" | paste -sd' ' -)
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

# C-11a inventory: use the same observable advisory/row-lock protocol as sealing.
inventory_fixture() {
  sql -v inventory_stock="${1:-1}" <<'SQL' | tail -1
SELECT set_config('race.stock',:'inventory_stock',false);
DO $f$ DECLARE t uuid; p uuid; s uuid; BEGIN
 INSERT INTO app.tenants(name) VALUES('inventory-race') RETURNING id INTO t;
 INSERT INTO app.products(tenant_id,product_code) VALUES(t,'inventory-race') RETURNING id INTO p;
 INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
  VALUES(t,p,'inventory-race','default') RETURNING id INTO s;
 PERFORM set_config('app.tenant_id',t::text,false);
 IF current_setting('race.stock')::bigint>0 THEN
  PERFORM app.inventory_adjust(s,current_setting('race.stock')::bigint,'race receipt',uuidv7(),'inbound');
 END IF;
 PERFORM set_config('race.inventory',concat_ws(' ',t,s,uuidv7()),false);
END $f$;
SELECT current_setting('race.inventory');
SQL
}
inventory_check() {  # physical/reserved/active-quantity/movement-count
  local got
  got=$(sql <<<"SELECT s.on_hand||'|'||s.reserved||'|'||
   (SELECT coalesce(sum(quantity),0) FROM app.inventory_reservations r WHERE r.tenant_id=s.tenant_id AND r.state='active')||'|'||
   (SELECT count(*) FROM app.inventory_movements m WHERE m.tenant_id=s.tenant_id)
   FROM app.stock_levels s WHERE s.tenant_id='$3' AND s.sku_id='$4';")
  echo "$1: $got (expected $2)"; [[ $got == "$2" ]] || fail=1
}
read -r T SKU OWNER <<<"$(inventory_fixture)"
gate_close 11
tx inv_first app_runtime "$T" "SELECT app.inventory_reserve('$SKU',1,'checkout','$OWNER','last-first',900); $(park 11)" &
await inv_first advisory
tx inv_second app_runtime "$T" "SELECT app.inventory_reserve('$SKU',1,'checkout','$OWNER','last-second',900);" &
await inv_second "${ROWLOCK[@]}"
gate_open 11; wait
expect inv_first ok; expect inv_second INVENTORY_INSUFFICIENT_STOCK; expect inv_second P0001
inventory_check 'race inventory last unit' '1|1|1|2' "$T" "$SKU"

for quantity in 1 2; do
  read -r T SKU OWNER <<<"$(inventory_fixture)"
  gate_close 12
  tx inv_key_first app_runtime "$T" "SELECT app.inventory_reserve('$SKU',1,'checkout','$OWNER','same-key',900); $(park 12)" &
  await inv_key_first advisory
  tx inv_key_second app_runtime "$T" "SELECT app.inventory_reserve('$SKU',$quantity,'checkout','$OWNER','same-key',900);" &
  await inv_key_second advisory
  gate_open 12; wait
  expect inv_key_first ok
  if [[ $quantity == 1 ]]; then expect inv_key_second ok
  else expect inv_key_second INVENTORY_IDEMPOTENCY_KEY_REUSED; expect inv_key_second 22023; fi
  inventory_check "race inventory same-key qty=$quantity" '1|1|1|2' "$T" "$SKU"
done

read -r T SKU OWNER <<<"$(inventory_fixture)"
RID=$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SELECT (app.inventory_reserve('$SKU',1,'order','$OWNER','expiry-race',900)).id;" | tail -1)
sql <<<"UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id='$T' AND id='$RID';" >/dev/null
gate_close 13
tx inv_expire app_runtime "$T" "SELECT app.inventory_expire_due(1); $(park 13)" &
await inv_expire advisory
tx inv_commit app_runtime "$T" "SELECT app.inventory_commit('$RID');" &
await inv_commit "${ROWLOCK[@]}"
gate_open 13; wait
expect inv_expire ok; expect inv_commit INVENTORY_INVALID_TRANSITION; expect inv_commit 55000
inventory_check 'race inventory expiry beats commit' '1|0|0|3' "$T" "$SKU"

# Commit owns the reservation but waits on stock; expiry must skip, not release
# its counter. Once the stock wait ends, the expired commit must refuse.
read -r T SKU OWNER <<<"$(inventory_fixture)"
RID=$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SELECT (app.inventory_reserve('$SKU',1,'order','$OWNER','skip-race',900)).id;" | tail -1)
sql <<<"UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id='$T' AND id='$RID';" >/dev/null
gate_close 14
tx inv_stock app_inventory_manager "$T" "SELECT app.inventory_adjust('$SKU',1,'concurrent receipt','$OWNER','inbound'); $(park 14)" &
await inv_stock advisory
tx inv_commit_wait app_runtime "$T" "SELECT app.inventory_commit('$RID');" &
await inv_commit_wait "${ROWLOCK[@]}"
SKIPPED=$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SET ROLE app_runtime; SELECT app.inventory_expire_due(1);" | tail -1)
[[ $SKIPPED == 0 ]] || fail=1
gate_open 14; wait
expect inv_stock ok; expect inv_commit_wait INVENTORY_RESERVATION_EXPIRED; expect inv_commit_wait 55000
sql <<<"SELECT set_config('app.tenant_id','$T',false); SET ROLE app_runtime; SELECT app.inventory_expire_due(1);" >/dev/null
inventory_check 'race inventory skipped busy commit then expiry' '2|0|0|4' "$T" "$SKU"

read -r T SKU OWNER <<<"$(inventory_fixture 0)"
gate_close 15
tx inv_init_first app_inventory_manager "$T" "SELECT app.inventory_adjust('$SKU',1,'initial receipt','$OWNER','inbound'); $(park 15)" &
await inv_init_first advisory
tx inv_init_second app_inventory_manager "$T" "SELECT app.inventory_adjust('$SKU',2,'second receipt','$OWNER','inbound');" &
await inv_init_second "${ROWLOCK[@]}"
gate_open 15; wait
expect inv_init_first ok; expect inv_init_second ok
inventory_check 'race inventory concurrent first warehouse+stock' '3|0|0|2' "$T" "$SKU"

if [[ $fail -ne 0 ]]; then echo "FAIL: race end state differs from the sealing contract" >&2; exit 1; fi
echo 'PASS: lock-synchronized seal/downgrade/child-append races keep sealed media public+ready'
echo 'PASS: inventory last-unit/key-replay/expiry-commit races preserve quantity and movement counts'
