#!/usr/bin/env bash
# Multi-connection gates for release sealing and reliability; real PG18 only.
# Depends on: an expendable DB already migrated by check_pg.sh; psql or Docker compose.
# Called by: CI after check_pg.sh. Invariant checked: a sealed release never
# references media that is non-public or whose blob is not ready, whatever the
# interleaving of runtime child insert, media downgrade and publish. Reliability
# claims must be disjoint, expired attempts fenced and same-key occupancy unique.
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
ORIGINAL_PSQL=("${PSQL[@]}")
BOOTSTRAP_DB=""
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
# $RACE_PREFIX<label> for the driver. Never aborts the gate; output kept per label.
RACE_PREFIX="race_$$_"
ERRDIR=$(mktemp -d)
cleanup() {  # never leave parked participants or gate sessions behind
  sql <<<"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE left(application_name,length('$RACE_PREFIX'))='$RACE_PREFIX';" >/dev/null 2>&1 || true
  PSQL=("${ORIGINAL_PSQL[@]}")
  [[ -z $BOOTSTRAP_DB ]] || sql -c "DROP DATABASE IF EXISTS $BOOTSTRAP_DB WITH (FORCE)" >/dev/null 2>&1 || true
  rm -rf "$ERRDIR"
}
trap cleanup EXIT
tx() {
  printf "SET application_name='${RACE_PREFIX}%s'; %s BEGIN; SELECT set_config('app.tenant_id','%s',true); SET LOCAL ROLE %s; %s COMMIT;\n" \
    "$1" "${5:-}" "$3" "$2" "$4" | sql >"$ERRDIR/$1.out" 2>"$ERRDIR/$1" || true
}
park() { echo "SELECT pg_advisory_xact_lock_shared($$, $1);"; } # SQL: wait for gate $1
gate_close() {  # driver session holds advisory lock $1 until gate_open
  echo "SET application_name='${RACE_PREFIX}gate_$1'; SELECT pg_advisory_lock($$, $1); SELECT pg_sleep(600);" | sql >/dev/null 2>&1 &
  # ClientRead can occur after SET but before pg_advisory_lock. Only the exact
  # granted two-int exclusive lock proves that a participant will park.
  local ready deadline=$((SECONDS + 15))
  while (( SECONDS < deadline )); do
    ready=$(sql <<<"SELECT count(*)=1 FROM pg_locks l JOIN pg_stat_activity a USING(pid)
      WHERE a.application_name='${RACE_PREFIX}gate_$1'
      AND l.locktype='advisory' AND l.mode='ExclusiveLock' AND l.granted
      AND l.classid=$$::oid AND l.objid=$1::oid AND l.objsubid=2;")
    [[ $ready == t ]] && return 0
    sleep 0.05
  done
  echo "  sync timeout: ${RACE_PREFIX}gate_$1 never held its exact advisory lock"; fail=1; return 1
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
  echo "  sync timeout: ${RACE_PREFIX}$label never reached ${*} (last: $ev)"; fail=1; return 1
}
await_job() {
  local label=$1 pid=$2 deadline=$((SECONDS + 15))
  while kill -0 "$pid" 2>/dev/null; do
    if (( SECONDS >= deadline )); then
      echo "  sync timeout: ${RACE_PREFIX}$label process did not finish"; fail=1; return 1
    fi
    sleep 0.05
  done
  wait "$pid"
}
ROWLOCK=(transactionid tuple)

# expect <label> <marker|ok>: winners must not error; losers only with their guard.
expect() {
  local err; err=$(cat "$ERRDIR/$1")
  if [[ $2 == ok ]]; then [[ -z $err ]] && return; else [[ $err == *"$2"* ]] && return; fi
  echo "  $1: expected ${2}, got: ${err:-no error}"; fail=1
}

result() {  # require an exact tagged result, including successful empty claims
  local line
  while IFS= read -r line; do
    [[ $line == "$2" ]] && return 0
  done <"$ERRDIR/$1.out"
  echo "  $1: missing result $2"; fail=1
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
# One tenant, four ready rows: A parks with two claims still uncommitted; B
# must claim the other two and COMMIT before A is released (SKIP LOCKED).
T=$(sql <<<"INSERT INTO app.tenants(name) VALUES('outbox-race') RETURNING id;" | tail -1)
read -r WA WB <<<"$(sql <<<"SELECT uuidv7(),uuidv7();" | tr '|' ' ')"
sql <<SQL >/dev/null
INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
 SELECT '$T','race','{}','race-worker',n::text FROM generate_series(1,4) n;
SQL
gate_close 4; gate_close 7
tx outbox_a app_outbox_worker "$T" "SELECT 'claimed='||count(*) FROM app.claim_outbox('$T','$WA',2,60); $(park 4)" &
await outbox_a advisory
tx outbox_b app_outbox_worker "$T" "SELECT 'claimed='||count(*) FROM app.claim_outbox('$T','$WB',2,60); $(park 7)" &
await outbox_b advisory
gate_open 7; await outbox_b gone
gate_open 4; wait
expect outbox_a ok; expect outbox_b ok
result outbox_a claimed=2; result outbox_b claimed=2
got=$(sql <<<"SELECT count(*)||'|'||count(*) FILTER (WHERE lease_owner='$WA')||'|'||count(*) FILTER (WHERE lease_owner='$WB')||'|'||bool_and(status='processing' AND attempts=1 AND lease_until>clock_timestamp()) FROM app.outbox_events WHERE tenant_id='$T';")
echo "race outbox disjoint claims: $got (expected 4|2|2|true)"
[[ $got == '4|2|2|true' ]] || fail=1

# Expiration is fixture data, not sleep ordering. Even before another worker
# reclaims, an expired attempt must not acknowledge or reschedule the row.
T=$(sql <<<"INSERT INTO app.tenants(name) VALUES('outbox-fence-race') RETURNING id;" | tail -1)
E=$(sql <<<"INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES('$T','race','{}','race-worker','fence') RETURNING id;" | tail -1)
tx lease_a app_outbox_worker "$T" "SELECT 'claimed='||count(*) FROM app.claim_outbox('$T','$WA',1,60);"
sql <<<"UPDATE app.outbox_events SET lease_until=clock_timestamp()-interval '1 second' WHERE tenant_id='$T' AND id='$E';" >/dev/null
tx expired app_outbox_worker "$T" "SELECT 'expired='||app.complete_outbox('$T','$E','$WA',1)||'|'||app.fail_outbox('$T','$E','$WA',1,'expired',0);"
expect lease_a ok; result lease_a claimed=1
expect expired ok; result expired 'expired=false|false'
gate_close 5
tx lease_b app_outbox_worker "$T" "SELECT 'claimed='||count(*) FROM app.claim_outbox('$T','$WB',1,60); $(park 5)" &
await lease_b advisory
tx stale app_outbox_worker "$T" "SELECT 'stale='||app.complete_outbox('$T','$E','$WA',1)||'|'||app.fail_outbox('$T','$E','$WA',1,'stale',0);" &
await stale "${ROWLOCK[@]}"
gate_open 5; wait
expect lease_b ok; result lease_b claimed=1
expect stale ok; result stale 'stale=false|false'
tx current app_outbox_worker "$T" "SELECT 'completed='||app.complete_outbox('$T','$E','$WB',2);"
expect current ok; result current completed=true
got=$(sql <<<"SELECT status||'|'||attempts||'|'||(delivered_at IS NOT NULL)||'|'||(last_error IS NULL) FROM app.outbox_events WHERE tenant_id='$T' AND id='$E';")
echo "race expired lease fencing: $got (expected delivered|2|true|true)"
[[ $got == 'delivered|2|true|true' ]] || fail=1

# The acknowledgement transaction starts while its lease is valid, then parks.
# Set expiry strictly between its observed start and current database clock:
# now() would wrongly accept the old lease; clock_timestamp() must reject it.
T=$(sql <<<"INSERT INTO app.tenants(name) VALUES('outbox-late-ack-race') RETURNING id;" | tail -1)
E=$(sql <<<"INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES('$T','race','{}','race-worker','late-ack') RETURNING id;" | tail -1)
tx late_claim app_outbox_worker "$T" "SELECT 'claimed='||count(*) FROM app.claim_outbox('$T','$WA',1,60);"
expect late_claim ok; result late_claim claimed=1
gate_close 8
tx late_ack app_outbox_worker "$T" "$(park 8) SELECT 'late='||app.complete_outbox('$T','$E','$WA',1)||'|'||app.fail_outbox('$T','$E','$WA',1,'late',0);" &
await late_ack advisory
got=$(sql <<<"UPDATE app.outbox_events e SET lease_until=a.xact_start+(clock_timestamp()-a.xact_start)/2 FROM pg_stat_activity a WHERE e.tenant_id='$T' AND e.id='$E' AND a.application_name='${RACE_PREFIX}late_ack' AND a.xact_start IS NOT NULL RETURNING (e.lease_until>a.xact_start AND e.lease_until<clock_timestamp());")
echo "race late ACK expiry position: $got (expected t)"
[[ $got == t ]] || fail=1
gate_open 8; wait
expect late_ack ok; result late_ack 'late=false|false'
got=$(sql <<<"SELECT status||'|'||attempts||'|'||(lease_owner='$WA')||'|'||(lease_until<clock_timestamp())||'|'||(delivered_at IS NULL)||'|'||(last_error IS NULL) FROM app.outbox_events WHERE tenant_id='$T' AND id='$E';")
echo "race late ACK transaction clock: $got (expected processing|1|true|true|true|true)"
[[ $got == 'processing|1|true|true|true|true' ]] || fail=1

# Upload expiry (media): the completing transaction waits on a row lock held
# past expires_at. Expiry must be judged by clock_timestamp() AFTER the lock is
# granted (second statement, mirroring queries/media.sql MediaUploadExpired);
# now() (transaction start) or a same-statement projection is stale.
T=$(sql <<<"INSERT INTO app.tenants(name) VALUES('upload-expiry-race') RETURNING id;" | tail -1)
U=$(sql <<<"INSERT INTO app.media_uploads(tenant_id,original_filename,expected_size,expires_at) VALUES('$T','x.png',1,clock_timestamp()+interval '3 seconds') RETURNING id;" | tail -1)
gate_close 9
tx up_holder app_media_service "$T" "SELECT 'held='||count(*) FROM (SELECT 1 FROM app.media_uploads WHERE tenant_id='$T' AND id='$U' FOR UPDATE) l; $(park 9)" &
await up_holder advisory
tx up_late app_media_service "$T" "SELECT 'locked='||count(*) FROM (SELECT 1 FROM app.media_uploads WHERE tenant_id='$T' AND id='$U' FOR UPDATE) l; SELECT 'expired='||(expires_at <= clock_timestamp()) FROM app.media_uploads WHERE tenant_id='$T' AND id='$U';" &
await up_late "${ROWLOCK[@]}"
for _ in $(seq 1 150); do
  [[ $(sql <<<"SELECT clock_timestamp() > expires_at FROM app.media_uploads WHERE id='$U';") == t ]] && break
  sleep 0.1
done
gate_open 9; wait
expect up_holder ok; expect up_late ok; result up_late locked=1; result up_late expired=true

# Same-key occupancy: the second UPSERT must wait for the uncommitted winner,
# then return its committed in_progress record with acquired=false.
T=$(sql <<<"INSERT INTO app.tenants(name) VALUES('idempotency-race') RETURNING id;" | tail -1)
read -r IA IB <<<"$(sql <<<"SELECT uuidv7(),uuidv7();" | tr '|' ' ')"
H=$(sql <<<"SELECT encode(sha256('same-request'::bytea),'hex');")
claim_a="INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,claim_token) VALUES('$T','service','$WA','race','same-key',sha256('same-request'::bytea),clock_timestamp()+interval '1 hour','$IA') ON CONFLICT (tenant_id,actor_kind,actor_id,operation,idempotency_key) DO UPDATE SET idempotency_key=EXCLUDED.idempotency_key RETURNING 'acquired='||(claim_token='$IA'::uuid)||'|'||state||'|'||encode(request_hash,'hex');"
claim_b=${claim_a//$IA/$IB}
gate_close 6
tx idem_a app_runtime "$T" "$claim_a $(park 6)" &
await idem_a advisory
tx idem_b app_runtime "$T" "$claim_b" &
await idem_b "${ROWLOCK[@]}"
gate_open 6; wait
expect idem_a ok; result idem_a "acquired=true|in_progress|$H"
expect idem_b ok; result idem_b "acquired=false|in_progress|$H"
got=$(sql <<<"SELECT count(*)||'|'||min(state)||'|'||bool_and(claim_token='$IA' AND request_hash=sha256('same-request'::bytea) AND response_status IS NULL AND response_body IS NULL AND response_body_reference IS NULL) FROM app.idempotency_records WHERE tenant_id='$T';")
echo "race same-key occupancy: $got (expected 1|in_progress|true)"
[[ $got == '1|in_progress|true' ]] || fail=1
# Crashed owner: two concurrent reclaimers of the same EXPIRED claim; exactly one
# takes it over (the other waits on the row lock, then sees a live claim).
T=$(sql <<<"INSERT INTO app.tenants(name) VALUES('idempotency-reclaim') RETURNING id;" | tail -1)
read -r IO IA IB <<<"$(sql <<<"SELECT uuidv7(),uuidv7(),uuidv7();" | tr '|' ' ')"
sql <<<"INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,claim_token) VALUES('$T','service','$WA','reclaim','k',sha256('r'::bytea),clock_timestamp()-interval '1 second','$IO');" >/dev/null
reclaim_a="SELECT 'owner='||(claim_token='$IA'::uuid) FROM app.idempotency_claim('$T','service','$WA','reclaim','k',sha256('r'::bytea),interval '1 hour','$IA');"
reclaim_b=${reclaim_a//$IA/$IB}
gate_close 7
tx rc_a app_runtime "$T" "$reclaim_a $(park 7)" &
await rc_a advisory
tx rc_b app_runtime "$T" "$reclaim_b" &
await rc_b "${ROWLOCK[@]}"
gate_open 7; wait
expect rc_a ok; result rc_a 'owner=true'
expect rc_b ok; result rc_b 'owner=false'
got=$(sql <<<"SELECT (claim_token='$IA')||'|'||(expires_at>clock_timestamp()) FROM app.idempotency_records WHERE tenant_id='$T';")
echo "race expired-claim takeover: $got (expected true|true)"
[[ $got == 'true|true' ]] || fail=1

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
tx inv_commit app_inventory_executor "$T" "SELECT app.inventory_commit('$RID');" &
await inv_commit "${ROWLOCK[@]}"
gate_open 13; wait
expect inv_expire ok; expect inv_commit INVENTORY_INVALID_TRANSITION; expect inv_commit 55000
inventory_check 'race inventory expiry beats commit' '1|0|0|3' "$T" "$SKU"

# Commit owns the reservation but waits on stock; expiry must skip, not release
# its counter. Its transaction and commit statement start before expiry; the
# call finishes after expiry. No scheduler must meet a short clock deadline.
read -r T SKU OWNER <<<"$(inventory_fixture)"
RID=$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SELECT (app.inventory_reserve('$SKU',1,'order','$OWNER','skip-race',900)).id;" | tail -1)
gate_close 14
tx inv_stock app_inventory_manager "$T" "SELECT app.inventory_adjust('$SKU',1,'concurrent receipt','$OWNER','inbound'); $(park 14)" &
await inv_stock advisory
# Park before acquiring the reservation, so the driver can put its expiry
# strictly between the worker's observed statement start and database clock.
# Both now() and statement_timestamp() would then admit it incorrectly. The
# materialized volatile barrier must complete before evaluating commit's call.
gate_close 114
tx inv_commit_wait app_inventory_executor "$T" "WITH barrier AS MATERIALIZED (SELECT pg_advisory_xact_lock_shared($$,114)) SELECT app.inventory_commit('$RID') FROM barrier;" &
await inv_commit_wait advisory
EXPIRY_POSITION=$(sql <<<"UPDATE app.inventory_reservations r
 SET expires_at=a.query_start+(clock_timestamp()-a.query_start)/2
 FROM pg_stat_activity a WHERE a.application_name='${RACE_PREFIX}inv_commit_wait'
 AND a.xact_start IS NOT NULL AND r.tenant_id='$T' AND r.id='$RID'
 RETURNING r.expires_at>a.xact_start AND r.expires_at>a.query_start AND r.expires_at<clock_timestamp();")
echo "race inventory expiry position: $EXPIRY_POSITION (expected t)"
[[ $EXPIRY_POSITION == t ]] || { echo 'FAIL: inventory transaction/statement/expiry clock witness missing'; exit 1; }
gate_open 114
await inv_commit_wait "${ROWLOCK[@]}"
STOCK_BLOCKER=$(sql <<<"SELECT count(*)=1 AND bool_and(h.pid=ANY(pg_blocking_pids(c.pid)))
 FROM pg_stat_activity c CROSS JOIN pg_stat_activity h
 WHERE c.application_name='${RACE_PREFIX}inv_commit_wait' AND h.application_name='${RACE_PREFIX}inv_stock';")
echo "race inventory commit blocked by stock holder: $STOCK_BLOCKER (expected t)"
[[ $STOCK_BLOCKER == t ]] || { echo 'FAIL: commit did not wait on the stock holder'; exit 1; }
SKIPPED=$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SET ROLE app_runtime; SELECT app.inventory_expire_due(1);" | tail -1)
echo "race inventory expiry skips busy commit: $SKIPPED (expected 0)"
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

# Expiry sweep must advance past a LOCKED oldest due reservation: with batch 1 it
# still expires another due row (it must not keep re-selecting the locked one).
read -r T SKU OWNER <<<"$(inventory_fixture 2)"
R1=$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SET ROLE app_runtime; SELECT (app.inventory_reserve('$SKU',1,'checkout','$OWNER','sweep-old',1)).id;" | tail -1)
R2=$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SET ROLE app_runtime; SELECT (app.inventory_reserve('$SKU',1,'checkout','$OWNER','sweep-new',2)).id;" | tail -1)
for _ in $(seq 1 100); do  # poll the database clock until both are due; never order actors by sleep
  [[ $(sql <<<"SELECT count(*) FROM app.inventory_reservations WHERE tenant_id='$T' AND expires_at<=clock_timestamp();") == 2 ]] && break
  sleep 0.05
done
gate_close 16
tx sweep_hold postgres "$T" "SELECT 1 FROM app.inventory_reservations WHERE id='$R1' FOR UPDATE; $(park 16)" &
await sweep_hold advisory
SWEPT=$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SET ROLE app_runtime; SELECT app.inventory_expire_due(1);" | tail -1)
gate_open 16; wait
echo "race expiry sweep skips locked oldest: expired=$SWEPT (expected 1)"
[[ $SWEPT == 1 ]] || fail=1
got=$(sql <<<"SELECT string_agg(state,',' ORDER BY expires_at) FROM app.inventory_reservations WHERE tenant_id='$T';")
echo "race expiry sweep states: $got (expected active,expired)"
[[ $got == 'active,expired' ]] || fail=1
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
gate_close 17
tx pricing_first postgres "$T" "$insert $(park 17)" &
await pricing_first advisory
tx pricing_second postgres "$T" "$insert" &
await pricing_second "${ROWLOCK[@]}"
gate_open 17; wait
expect pricing_first ok; expect pricing_second price_entries_tier_unique
got=$(sql <<<"SELECT count(*) FROM app.price_entries WHERE tenant_id='$T' AND price_list_id='$L' AND sku_id='$K' AND min_quantity=1;")
[[ $got == 1 ]] || { echo "FAIL: pricing race persisted $got duplicate tiers"; fail=1; }
echo "pricing duplicate-tier race: $got committed tier (expected 1)"
# Shipping exclusion must protect writes from different connections, including
# UPDATE vs INSERT. No timing assumption: first holds its range exclusion lock
# at an advisory gate until the driver observes the second transaction blocked.
for scenario in insert_low_first insert_high_first update_first insert_before_update; do
 read -r T Z P <<<"$(sql <<'SQL' | tail -1
INSERT INTO app.tenants(name) VALUES('shipping-race') RETURNING id AS t \gset
INSERT INTO app.shipping_zones(tenant_id,name) VALUES(:'t','R') RETURNING id AS z \gset
INSERT INTO app.shipping_profiles(tenant_id,name,currency) VALUES(:'t','R','USD') RETURNING id AS p \gset
SELECT :'t'||' '||:'z'||' '||:'p';
SQL
)"
 low="INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,min_weight_g,max_weight_g) VALUES('$T','$P','$Z','by_weight','USD',10,0,500);"
 high="INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,min_weight_g,max_weight_g) VALUES('$T','$P','$Z','by_weight','USD',20,499,1000);"
 expected=1
 case $scenario in
  insert_low_first) first=$low; second=$high; want='0:500' ;;
  insert_high_first) first=$high; second=$low; want='499:1000' ;;
  *) sql <<<"$low" >/dev/null
     up="UPDATE app.shipping_rates SET max_weight_g=700 WHERE tenant_id='$T' AND profile_id='$P' AND zone_id='$Z';"
     ins="INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,min_weight_g,max_weight_g) VALUES('$T','$P','$Z','by_weight','USD',20,600,1000);"
     if [[ $scenario == update_first ]]; then first=$up; second=$ins; want='0:700'
     else first=$ins; second=$up; want='0:500,600:1000'; expected=2; fi ;;
 esac
 gate_close 20
 tx shipping_first postgres "$T" "$first $(park 20)" &
 await shipping_first advisory
 tx shipping_second postgres "$T" "$second" &
 await shipping_second "${ROWLOCK[@]}"
 gate_open 20; wait
 expect shipping_first ok; expect shipping_second shipping_weight_no_overlap; expect shipping_second 23P01
 got=$(sql <<<"SELECT count(*)||'|'||string_agg(min_weight_g||':'||max_weight_g,',' ORDER BY min_weight_g) FROM app.shipping_rates WHERE tenant_id='$T' AND profile_id='$P' AND zone_id='$Z';")
 echo "shipping race $scenario: $got (expected $expected|$want)"
 [[ $got == "$expected|$want" ]] || fail=1
done


# Concurrent INSERTs and UPDATEs must serialize on the EXCLUDE constraint.
# Driver parks the winner after its write, witnesses the contender blocked on
# its transaction, then releases it; no timed ordering or silent generic error.
T=$(sql <<<"INSERT INTO app.tenants(name) VALUES('tax-window-race') RETURNING id;" | tail -1)
Z=$(sql <<<"INSERT INTO app.tax_zones(tenant_id,name) VALUES('$T','US') RETURNING id;" | tail -1)
sql <<<"INSERT INTO app.tax_categories(tenant_id,code,name) VALUES('$T','standard','Standard');" >/dev/null
rate_a="INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from,valid_until) VALUES('$T','$Z','standard',500,'2026-01-01Z','2027-01-01Z');"
rate_b="INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from,valid_until) VALUES('$T','$Z','standard',600,'2026-06-01Z','2027-06-01Z');"
gate_close 21
tx tax_a app_runtime "$T" "$rate_a $(park 21)" &
await tax_a advisory
tx tax_b app_runtime "$T" "$rate_b" &
await tax_b "${ROWLOCK[@]}"
gate_open 21; wait
expect tax_a ok; expect tax_b tax_rate_no_overlap
got=$(sql <<<"SELECT count(*)||'|'||min(rate_bp) FROM app.tax_rates WHERE tenant_id='$T';")
echo "race tax overlapping inserts: $got (expected 1|500)"
[[ $got == '1|500' ]] || fail=1
# Both original windows are valid and disjoint. Concurrent expansions overlap
# each other although the losing expansion was valid against the old winner.
sql <<<"UPDATE app.tax_rates SET valid_until='2026-02-01Z' WHERE tenant_id='$T'; INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from,valid_until) VALUES('$T','$Z','standard',600,'2026-03-01Z','2026-04-01Z');" >/dev/null
gate_close 22
tx tax_up_a app_runtime "$T" "UPDATE app.tax_rates SET valid_until='2026-02-20Z' WHERE tenant_id='$T' AND rate_bp=500; $(park 22)" &
await tax_up_a advisory
tx tax_up_b app_runtime "$T" "UPDATE app.tax_rates SET valid_from='2026-02-10Z' WHERE tenant_id='$T' AND rate_bp=600;" &
await tax_up_b "${ROWLOCK[@]}"
gate_open 22; wait
expect tax_up_a ok; expect tax_up_b tax_rate_no_overlap
got=$(sql <<<"SELECT count(*)||'|'||bool_and(CASE rate_bp WHEN 500 THEN valid_until='2026-02-20Z'::timestamptz ELSE valid_from='2026-03-01Z'::timestamptz END) FROM app.tax_rates WHERE tenant_id='$T';")
echo "race tax overlapping updates: $got (expected 2|true)"
[[ $got == '2|true' ]] || fail=1

# Customer registration uses its own restricted auth role, never app_auth/users.
# A's INSERT owns the unique-index entry while parked; B must wait on that
# transaction before A commits. A valid failure includes both SQLSTATE and the
# intended constraint, so permissions/fixture errors cannot count as a loser.
T=$(sql <<<"INSERT INTO app.tenants(name) VALUES('customer-register-race') RETURNING id;" | tail -1)
gate_close 23
tx customer_first app_customer_auth "$T" "INSERT INTO app.customers(tenant_id,email,display_name) VALUES('$T',' Race.Customer@Example.COM ','first'); $(park 23)" &
await customer_first advisory
tx customer_second app_customer_auth "$T" "INSERT INTO app.customers(tenant_id,email,display_name) VALUES('$T','race.customer@example.com','second');" &
await customer_second "${ROWLOCK[@]}"
gate_open 23; wait
expect customer_first ok
expect customer_second customers_tenant_email_unique
expect customer_second 23505
got=$(sql <<<"SELECT count(*)||'|'||min(display_name) FROM app.customers WHERE tenant_id='$T' AND email_normalized='race.customer@example.com';")
echo "customer normalized registration race: $got (expected 1|first)"
[[ $got == '1|first' ]] || fail=1

# Equal normalized addresses in two tenants are independent identities. B must
# commit while A is still parked, proving uniqueness is tenant-scoped.
read -r TA TB <<<"$(sql <<'SQL'
INSERT INTO app.tenants(name) VALUES('customer-independent-A') RETURNING id AS a \gset
INSERT INTO app.tenants(name) VALUES('customer-independent-B') RETURNING id AS b \gset
SELECT :'a'||' '||:'b';
SQL
)"
gate_close 24
tx customer_tenant_a app_customer_auth "$TA" "INSERT INTO app.customers(tenant_id,email,display_name) VALUES('$TA',' Both@Example.COM ','tenant-A'); $(park 24)" &
await customer_tenant_a advisory
tx customer_tenant_b app_customer_auth "$TB" "INSERT INTO app.customers(tenant_id,email,display_name) VALUES('$TB','both@example.com','tenant-B');" &
customer_tenant_b_pid=$!
await_job customer_tenant_b "$customer_tenant_b_pid"
expect customer_tenant_b ok
got=$(sql <<<"SELECT count(*) FROM app.customers WHERE tenant_id='$TB' AND email_normalized='both@example.com';")
[[ $got == 1 ]] || { echo "FAIL: other-tenant registration did not commit before gate release: $got"; fail=1; }
gate_open 24; wait
expect customer_tenant_a ok
got=$(sql <<<"SELECT count(*)||'|'||count(DISTINCT tenant_id) FROM app.customers WHERE tenant_id IN('$TA','$TB') AND email_normalized='both@example.com';")
echo "customer independent-tenant registration race: $got (expected 2|2)"
[[ $got == '2|2' ]] || fail=1
# Ledger atomic posting: both participants use the real runtime role on their
# own connections. Winner parks with its journal uncommitted; the contender
# must be observed waiting on its transaction before release. Repeat with
# identical content and changed amount, across all supported isolation levels.
for isolation in 'READ COMMITTED' 'REPEATABLE READ' 'SERIALIZABLE'; do
 for scenario in identical changed; do
  read -r T A B <<<"$(sql <<'SQL' | tail -1
INSERT INTO app.tenants(name,settlement_currency) VALUES('ledger-race','USD') RETURNING id AS t \gset
INSERT INTO app.ledger_accounts(tenant_id,code,account_type,currency)
 VALUES(:'t','cash','asset','USD') RETURNING id AS a \gset
INSERT INTO app.ledger_accounts(tenant_id,code,account_type,currency)
 VALUES(:'t','revenue','revenue','USD') RETURNING id AS b \gset
SELECT :'t'||' '||:'a'||' '||:'b';
SQL
)"
  amount=100; [[ $scenario == changed ]] && amount=101
  post="SELECT 'journal='||app.post_ledger('$T','USD','race','same-key','capture',ARRAY['$A','$B']::uuid[],ARRAY['debit','credit'],ARRAY[100,100]::bigint[]);"
  contender="SELECT 'journal='||app.post_ledger('$T','USD','race','same-key','capture',ARRAY['$A','$B']::uuid[],ARRAY['debit','credit'],ARRAY[$amount,$amount]::bigint[]);"
  gate_close 30
  tx ledger_first app_runtime "$T" "$post $(park 30)" "SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL $isolation;" &
  await ledger_first advisory
  tx ledger_second app_runtime "$T" "$contender" "SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL $isolation;" &
  await ledger_second "${ROWLOCK[@]}"
  gate_open 30; wait
  expect ledger_first ok
  if [[ $isolation != 'READ COMMITTED' ]]; then
   # PostgreSQL may reject a stale snapshot; replay through a new transaction
   # proves the API result after the concurrent winner is visible.
   expect ledger_second 40001
   tx ledger_second app_runtime "$T" "$contender"
  fi
  if [[ $scenario == identical ]]; then
   expect ledger_second ok
   first_id=$(sed -n 's/^journal=//p' "$ERRDIR/ledger_first.out")
   [[ -n $first_id ]] || { echo 'FAIL: ledger winner returned no journal id'; fail=1; }
   result ledger_second "journal=$first_id"
  else expect ledger_second LEDGER_IDEMPOTENCY_CONTENT; expect ledger_second 23505; fi
  got=$(sql <<<"SELECT count(*)||'|'||(SELECT count(*) FROM app.ledger_entries WHERE tenant_id='$T')||'|'||bool_and(status='posted' AND posted_at IS NOT NULL) FROM app.ledger_transactions WHERE tenant_id='$T';")
  echo "ledger race $isolation/$scenario: $got (expected 1|2|true)"
  [[ $got == '1|2|true' ]] || fail=1
 done
done

# Catalog optimistic locking: a stale transaction waits on the winner's row
# lock, then rechecks version at READ COMMITTED and must update zero rows.
# Gate 31 is distinct from prior module synchronization keys.
T=$(sql <<<"INSERT INTO app.tenants(name) VALUES('catalog-cas-race') RETURNING id;" | tail -1)
P=$(sql <<<"INSERT INTO app.products(tenant_id,product_code) VALUES('$T','CAS-RACE') RETURNING id;" | tail -1)
cas_first="WITH changed AS (UPDATE app.products SET product_code='CAS-FIRST',version=version+1 WHERE tenant_id='$T' AND id='$P' AND version=1 RETURNING id) SELECT 'C07_CAS_FIRST='||count(*) FROM changed;"
cas_stale="WITH changed AS (UPDATE app.products SET product_code='CAS-STALE',version=version+1 WHERE tenant_id='$T' AND id='$P' AND version=1 RETURNING id) SELECT 'C07_CAS_STALE='||count(*) FROM changed;"
gate_close 31
tx catalog_cas_first app_runtime "$T" "$cas_first $(park 31)" &
await catalog_cas_first advisory
tx catalog_cas_stale app_runtime "$T" "$cas_stale" &
await catalog_cas_stale "${ROWLOCK[@]}"
gate_open 31; wait
expect catalog_cas_first ok; expect catalog_cas_stale ok
result catalog_cas_first 'C07_CAS_FIRST=1'; result catalog_cas_stale 'C07_CAS_STALE=0'
got=$(sql <<<"SELECT version||'|'||product_code FROM app.products WHERE tenant_id='$T' AND id='$P';")
echo "race catalog stale CAS: $got (expected 2|CAS-FIRST)"
[[ $got == '2|CAS-FIRST' ]] || fail=1

# Cart CAS: loser waits on the actual cart row while winner is parked on31.
# The stale contender may not modify lines; the same transaction owns lock/CAS.
read -r T C S <<<"$(sql <<'SQL' | tail -1
INSERT INTO app.tenants(name) VALUES('cart-cas-race') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code,lifecycle_status) VALUES(:'t','cart-race','ready') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'p','cart-race','default') RETURNING id AS s \gset
INSERT INTO app.carts(tenant_id,guest_token_hash,currency) VALUES(:'t',sha256('race-cart-guest'::bytea),'USD') RETURNING id AS c \gset
INSERT INTO app.cart_lines VALUES(:'t',:'c',:'s',1);
SELECT :'t'||' '||:'c'||' '||:'s';
SQL
)"
ctx="SELECT set_config('app.cart_customer_id','',true),set_config('app.cart_token_hash',encode(sha256('race-cart-guest'::bytea),'hex'),true);"
cas="UPDATE app.carts SET version=version+1 WHERE tenant_id='$T' AND id='$C' AND version=1 RETURNING id"
gate_close 31
tx cart_first app_runtime "$T" "$ctx SELECT version FROM app.carts WHERE tenant_id='$T' AND id='$C' FOR UPDATE; WITH bump AS ($cas), changed AS (UPDATE app.cart_lines SET quantity=4 WHERE tenant_id='$T' AND cart_id='$C' AND sku_id='$S' AND EXISTS(SELECT FROM bump) RETURNING sku_id) SELECT 'CART_CAS='||count(*) FROM changed; $(park 31)" &
cart_first_pid=$!
await cart_first advisory
tx cart_second app_runtime "$T" "$ctx SELECT version FROM app.carts WHERE tenant_id='$T' AND id='$C' FOR UPDATE; WITH bump AS ($cas), changed AS (UPDATE app.cart_lines SET quantity=5 WHERE tenant_id='$T' AND cart_id='$C' AND sku_id='$S' AND EXISTS(SELECT FROM bump) RETURNING sku_id) SELECT 'CART_CAS='||count(*) FROM changed;" &
cart_second_pid=$!
await cart_second "${ROWLOCK[@]}"
gate_open 31
await cart_first gone; await cart_second gone
wait "$cart_first_pid"; wait "$cart_second_pid"
expect cart_first ok; expect cart_second ok
result cart_first 'CART_CAS=1'; result cart_second 'CART_CAS=0'
got=$(sql <<<"SELECT c.version||'|'||l.quantity FROM app.carts c JOIN app.cart_lines l ON (l.tenant_id,l.cart_id)=(c.tenant_id,c.id) WHERE c.tenant_id='$T' AND c.id='$C';")
echo "race cart lock/CAS: $got (expected 2|4)"
[[ $got == '2|4' ]] || fail=1

# C-15a: both commands target one sealed order and its exact owned reservation.
# Park the first after its transition, witness the contender's order-row wait,
# then release. RC rejects the obsolete command; RR/Serializable serialize-fail.
# Each level is exercised with both winners; stock, movements and outbox counts
# are positive witnesses, including rollback of every losing side effect.
for isolation in 'READ COMMITTED' 'REPEATABLE READ' SERIALIZABLE; do
 for first_target in paid cancelled; do
  read -r T SKU OID <<<"$(inventory_fixture)"
  sql <<SQL >/dev/null
SELECT set_config('app.tenant_id','$T',false);
SET ROLE app_runtime;
SELECT (app.inventory_reserve('$SKU',1,'order','$OID','orders-race-reserve',900)).id AS rid \gset
SELECT app.orders_create('$OID','orders-race-create',sha256('race'::bytea),NULL,
 '{"email":"race@example.invalid"}','USD',20,'{"amount_minor":20,"currency":"USD","service":"fixture"}',
 jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id','$SKU','sku','race-sku','quantity',1,
 'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',10,'line_total_minor',110,
 'reservation_id',:'rid','tax_snapshot',jsonb_build_object('net_minor',100,'tax_minor',10,'gross_minor',110,'currency','USD'))),'[]');
SQL
  if [[ $first_target == paid ]]; then second_target=cancelled; stock=0; race_gate=40
  else second_target=paid; stock=1; race_gate=41; fi
  gate_close "$race_gate"
  tx orders_first app_runtime "$T" "SELECT app.orders_transition('$OID','$first_target','race-first',0); $(park "$race_gate")" "SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL $isolation;" &
  await orders_first advisory
  tx orders_second app_runtime "$T" "SELECT app.orders_transition('$OID','$second_target','race-second',0);" "SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL $isolation;" &
  await orders_second "${ROWLOCK[@]}"
  gate_open "$race_gate"; wait
  expect orders_first ok
  if [[ $isolation == 'READ COMMITTED' ]]; then expect orders_second ORDER_INVALID_TRANSITION; expect orders_second 55000
  else expect orders_second 40001; fi
  got=$(sql <<<"SELECT o.state||'|'||o.version||'|'||s.on_hand||'|'||s.reserved||'|'||
   (SELECT count(*) FROM app.inventory_movements WHERE tenant_id='$T')||'|'||
   (SELECT count(*) FROM app.outbox_events WHERE tenant_id='$T' AND payload->>'order_id'='$OID')
   FROM app.orders o JOIN app.stock_levels s USING(tenant_id) WHERE o.tenant_id='$T' AND o.id='$OID';")
  echo "orders pay/cancel $isolation first=$first_target: $got (expected $first_target|2|$stock|0|3|2)"
  [[ $got == "$first_target|2|$stock|0|3|2" ]] || fail=1
 done
done

# C-15a: a two-line order must lock EVERY reservation before its first stock
# mutation. The first commit parks in a test-only movement trigger; another
# connection proves the later reservation is already locked (NOWAIT 55P03).
# A due sweep skips that held later reservation. Payment then notices expiry
# and rolls all changes back; subsequent expiry+cancel preserve physical stock.
read -r T SKU OID <<<"$(inventory_fixture 2)"
sql <<SQL >/dev/null
SELECT set_config('app.tenant_id','$T',false);
SET ROLE app_runtime;
SELECT (app.inventory_reserve('$SKU',1,'order','$OID','prelock-first',900)).id AS r1 \gset
SELECT (app.inventory_reserve('$SKU',1,'order','$OID','prelock-second',900)).id AS r2 \gset
SELECT app.orders_create('$OID','prelock-create',sha256('prelock'::bytea),NULL,
 '{"email":"prelock@example.invalid"}','USD',0,'{}',
 jsonb_build_array(
  jsonb_build_object('id',uuidv7(),'sku_id','$SKU','sku','prelock-sku','quantity',1,
   'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,
   'reservation_id',:'r1','tax_snapshot','{}'::jsonb),
  jsonb_build_object('id',uuidv7(),'sku_id','$SKU','sku','prelock-sku','quantity',1,
   'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,
   'reservation_id',:'r2','tax_snapshot','{}'::jsonb)), '[]');
SQL
read -r R_FIRST R_LATER <<<"$(sql <<<"SELECT id FROM app.inventory_reservations WHERE tenant_id='$T' AND owner_id='$OID' ORDER BY id;" | paste -sd' ' -)"
sql <<SQL >/dev/null
UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second'
 WHERE tenant_id='$T' AND id='$R_LATER';
CREATE FUNCTION public.c15a_prelock_$$() RETURNS trigger LANGUAGE plpgsql AS \$body\$
BEGIN
 PERFORM pg_advisory_xact_lock_shared($$,42);
 RETURN NEW;
END \$body\$;
CREATE TRIGGER c15a_prelock_$$ BEFORE INSERT ON app.inventory_movements
 FOR EACH ROW WHEN (NEW.kind='commit' AND NEW.reservation_id='$R_FIRST')
 EXECUTE FUNCTION public.c15a_prelock_$$();
SQL
gate_close 42
tx orders_prelock_pay app_runtime "$T" "SELECT app.orders_transition('$OID','paid','prelock-pay',0);" &
await orders_prelock_pay advisory
tx orders_prelock_probe postgres "$T" "DO \$proof\$ DECLARE locked boolean:=false; BEGIN
 BEGIN PERFORM id FROM app.inventory_reservations WHERE tenant_id='$T' AND id='$R_LATER' FOR UPDATE NOWAIT;
 EXCEPTION WHEN lock_not_available THEN locked:=true; END;
 IF NOT locked THEN RAISE EXCEPTION 'ORDERS_PRELOCK_GATE reservation not locked before stock write'; END IF;
 END \$proof\$;"
expect orders_prelock_probe ok
# In the intentionally broken overlay, NOWAIT succeeds. Do not start a sweep
# that would deadlock: report the exact fault, then release the driver normally.
if [[ ! -s "$ERRDIR/orders_prelock_probe" ]]; then
 tx orders_prelock_sweep app_runtime "$T" "SELECT 'skipped='||app.inventory_expire_due(100);"
 expect orders_prelock_sweep ok; result orders_prelock_sweep 'skipped=0'
fi
gate_open 42; wait
expect orders_prelock_pay INVENTORY_RESERVATION_EXPIRED; expect orders_prelock_pay 55000
sql <<SQL >/dev/null
DROP TRIGGER c15a_prelock_$$ ON app.inventory_movements;
DROP FUNCTION public.c15a_prelock_$$();
SQL
got=$(sql <<<"SELECT o.state||'|'||o.version||'|'||s.on_hand||'|'||s.reserved||'|'||
 (SELECT count(*) FROM app.inventory_movements WHERE tenant_id='$T')||'|'||
 (SELECT count(*) FROM app.outbox_events WHERE tenant_id='$T' AND payload->>'order_id'='$OID')
 FROM app.orders o JOIN app.stock_levels s USING(tenant_id) WHERE o.tenant_id='$T' AND o.id='$OID';")
echo "orders prelock/expiry rollback: $got (expected pending_payment|1|2|2|3|1)"
[[ $got == 'pending_payment|1|2|2|3|1' ]] || fail=1
tx orders_prelock_cancel app_runtime "$T" "SELECT app.inventory_expire_due(100); SELECT app.orders_transition('$OID','cancelled','prelock-cancel',0);"
expect orders_prelock_cancel ok
got=$(sql <<<"SELECT o.state||'|'||s.on_hand||'|'||s.reserved||'|'||
 (SELECT count(*) FROM app.inventory_movements WHERE tenant_id='$T')||'|'||
 (SELECT count(*) FROM app.outbox_events WHERE tenant_id='$T' AND payload->>'order_id'='$OID')
 FROM app.orders o JOIN app.stock_levels s USING(tenant_id) WHERE o.tenant_id='$T' AND o.id='$OID';")
echo "orders prelock expiry/cancel conservation: $got (expected cancelled|2|0|5|2)"
[[ $got == 'cancelled|2|0|5|2' ]] || fail=1

# C-02a: global auth has no tenant context. The first call owns the normalized
# identifier lock, then parks at gate70; the second must block on the first's
# auth lock before the driver releases the gate. No sleep establishes ordering.
auth_tx() {
 printf "SET application_name='${RACE_PREFIX}%s'; BEGIN; SET LOCAL ROLE app_auth; %s COMMIT;\n" \
  "$1" "$2" | sql >"$ERRDIR/$1.out" 2>"$ERRDIR/$1" || true
}
IDENTIFIER="${RACE_PREFIX}auth@example.test"
UPPER_IDENTIFIER=$(printf '%s' "$IDENTIFIER" | tr '[:lower:]' '[:upper:]')
record_first="SELECT 'auth-count='||failure_count FROM app.auth_record_failure('  $UPPER_IDENTIFIER  ','192.0.2.18'::inet);"
record_second="SELECT 'auth-count='||failure_count FROM app.auth_record_failure('$IDENTIFIER','192.0.2.18'::inet);"
gate_close 70
auth_tx auth_first "$record_first $(park 70)" &
auth_first_pid=$!
await auth_first advisory
auth_tx auth_second "$record_second" &
auth_second_pid=$!
await auth_second advisory
blocked=$(sql <<<"SELECT first.pid=ANY(pg_blocking_pids(second.pid)) FROM pg_stat_activity first,pg_stat_activity second WHERE first.application_name='${RACE_PREFIX}auth_first' AND second.application_name='${RACE_PREFIX}auth_second';")
[[ $blocked == t ]] || { echo "FAIL: auth second did not block on first identifier lock: $blocked"; fail=1; }
gate_open 70
wait "$auth_first_pid" "$auth_second_pid"
expect auth_first ok; expect auth_second ok
result auth_first 'auth-count=1'; result auth_second 'auth-count=2'
got=$(sql <<<"SELECT a.failure_count||'|'||i.failure_count FROM app.auth_account_attempts a JOIN app.auth_login_attempts i USING(identifier_normalized) WHERE a.identifier_normalized='$IDENTIFIER' AND i.ip='192.0.2.18'::inet;")
echo "race auth account+IP failure increments: $got (expected 2|2)"
[[ $got == '2|2' ]] || fail=1

# Cleanup must skip both stale rows while an ordinary failure waits for their
# row locks, then preserve the renewed failures once the holder commits.
PRUNE_IDENTIFIER="${RACE_PREFIX}prune@example.test"
sql <<SQL >/dev/null
INSERT INTO app.auth_account_attempts(identifier_normalized,failure_count,window_started_at,cooldown_until,last_failed_at)
 VALUES('$PRUNE_IDENTIFIER',4,now()-interval '2 days',now()-interval '1 day',now()-interval '2 days');
INSERT INTO app.auth_login_attempts(identifier_normalized,ip,failure_count,window_started_at,cooldown_until,last_failed_at)
 VALUES('$PRUNE_IDENTIFIER','192.0.2.19',4,now()-interval '2 days',now()-interval '1 day',now()-interval '2 days');
SQL
gate_close 72
tx prune_hold postgres "$T" "SELECT 1 FROM app.auth_account_attempts WHERE identifier_normalized='$PRUNE_IDENTIFIER' FOR UPDATE; SELECT 1 FROM app.auth_login_attempts WHERE identifier_normalized='$PRUNE_IDENTIFIER' FOR UPDATE; $(park 72)" &
prune_hold_pid=$!
await prune_hold advisory
auth_tx prune_waiter "SELECT 'renewed='||failure_count FROM app.auth_record_failure('$PRUNE_IDENTIFIER','192.0.2.19'::inet);" &
prune_waiter_pid=$!
await prune_waiter "${ROWLOCK[@]}"
blocked=$(sql <<<"SELECT first.pid=ANY(pg_blocking_pids(second.pid)) FROM pg_stat_activity first,pg_stat_activity second WHERE first.application_name='${RACE_PREFIX}prune_hold' AND second.application_name='${RACE_PREFIX}prune_waiter';")
[[ $blocked == t ]] || { echo "FAIL: auth renewal did not wait on held stale row: $blocked"; fail=1; }
auth_tx prune_skip "SELECT 'pruned='||accounts_deleted||'|'||ips_deleted FROM app.auth_prune_attempts(1000);"
expect prune_skip ok; result prune_skip 'pruned=0|0'
gate_open 72
wait "$prune_hold_pid" "$prune_waiter_pid"
expect prune_hold ok; expect prune_waiter ok; result prune_waiter 'renewed=1'
auth_tx prune_live "SELECT 'pruned='||accounts_deleted||'|'||ips_deleted FROM app.auth_prune_attempts(1000);"
expect prune_live ok; result prune_live 'pruned=0|0'
got=$(sql <<<"SELECT a.failure_count||'|'||i.failure_count||'|'||(a.last_failed_at>now()-interval '1 minute' AND i.last_failed_at>now()-interval '1 minute') FROM app.auth_account_attempts a JOIN app.auth_login_attempts i USING(identifier_normalized) WHERE a.identifier_normalized='$PRUNE_IDENTIFIER' AND i.ip='192.0.2.19'::inet;")
echo "race auth cleanup skips locks and preserves renewed rows: $got (expected 1|1|true)"
[[ $got == '1|1|true' ]] || fail=1

# First-owner is installation-wide: use an empty independently migrated database,
# because earlier gates legitimately created owners. Preserve the caller's psql
# target and clean our database even on a synchronization or migration failure.
BOOTSTRAP_DB=bootstrap_race_$$_$RANDOM
sql -c "CREATE DATABASE $BOOTSTRAP_DB" >/dev/null
if [[ -n "${DATABASE_URL:-}" ]]; then
 bootstrap_base="${DATABASE_URL%%\?*}"; bootstrap_query=""
 [[ $DATABASE_URL != *\?* ]] || bootstrap_query="?${DATABASE_URL#*\?}"
 PSQL=(psql "${bootstrap_base%/*}/$BOOTSTRAP_DB$bootstrap_query" -X -q -At -v ON_ERROR_STOP=1 -v VERBOSITY=verbose)
else
 PSQL=(docker compose -f deploy/compose.yaml exec -T postgres psql -X -q -At -U postgres -d "$BOOTSTRAP_DB" -v ON_ERROR_STOP=1 -v VERBOSITY=verbose)
fi
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 # Queue authorization depends on River's schema in this fresh database too.
 if [[ $f == *_queue_authorization.sql ]]; then
  if [[ -n "${DATABASE_URL:-}" ]]; then
   DATABASE_URL="${bootstrap_base%/*}/$BOOTSTRAP_DB$bootstrap_query" ./scripts/migrate_river.sh
  else
   RIVER_DATABASE_NAME="$BOOTSTRAP_DB" ./scripts/migrate_river.sh
  fi
 fi
 { echo 'BEGIN;'; cat "$f"; echo 'COMMIT;'; } | sql >/dev/null
done
bootstrap_tx() {
 printf "SET application_name='${RACE_PREFIX}%s'; BEGIN ISOLATION LEVEL READ COMMITTED; SET LOCAL ROLE app_admin_bootstrap; %s COMMIT;\n" \
  "$1" "$2" | sql >"$ERRDIR/$1.out" 2>"$ERRDIR/$1" || true
}
# Fixed, non-secret PHC-shaped fixture; never a real credential.
BOOTSTRAP_PHC='$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
gate_close 71
bootstrap_tx bootstrap_first "SELECT app.bootstrap_admin('first-owner@example.test','first owner','$BOOTSTRAP_PHC'); $(park 71)" &
bootstrap_first_pid=$!
await bootstrap_first advisory
bootstrap_tx bootstrap_second "SELECT app.bootstrap_admin('second-owner@example.test','second owner','$BOOTSTRAP_PHC');" &
bootstrap_second_pid=$!
await bootstrap_second advisory
blocked=$(sql <<<"SELECT first.pid=ANY(pg_blocking_pids(second.pid)) FROM pg_stat_activity first,pg_stat_activity second WHERE first.application_name='${RACE_PREFIX}bootstrap_first' AND second.application_name='${RACE_PREFIX}bootstrap_second';")
[[ $blocked == t ]] || { echo "FAIL: bootstrap second did not block on first installation lock: $blocked"; fail=1; }
gate_open 71
wait "$bootstrap_first_pid" "$bootstrap_second_pid"
expect bootstrap_first ok; expect bootstrap_second BOOTSTRAP_OWNER_EXISTS; expect bootstrap_second 55000
got=$(sql <<<"SELECT 'BOOTSTRAP_RACE='||(SELECT count(*) FROM app.tenant_memberships WHERE role='owner')||'|'||(SELECT count(*) FROM app.users)||'|'||(SELECT count(*) FROM app.tenants);")
echo "race first-owner bootstrap: $got (expected BOOTSTRAP_RACE=1|1|1)"
[[ $got == 'BOOTSTRAP_RACE=1|1|1' ]] || fail=1
PSQL=("${ORIGINAL_PSQL[@]}")
sql -c "DROP DATABASE $BOOTSTRAP_DB" >/dev/null
BOOTSTRAP_DB=""


# C-02b: execute the production sqlc rotation/insert statements. The UPDATE's
# zero-row result aborts the transaction before INSERT, matching SaveLogin.
# Extract named queries instead of cloning predicates that could silently drift.
login_rotation_sql() {
 python3 - "$1" "$2" "$3" <<'PYSQL'
from pathlib import Path
import re
import sys
user_id, old_hash, new_hash = sys.argv[1:]
assert re.fullmatch(r'[0-9a-f-]{36}', user_id)
assert all(re.fullmatch(r'[0-9a-f]{64}', h) for h in (old_hash, new_hash))
source = Path('queries/identity/login.sql').read_text()
def query(name, token_hash):
    match = re.search(r'^-- name: '+name+r' :\w+\n(.*?)(?=^-- name:|\Z)', source, re.M | re.S)
    assert match, name
    sql = match[1].strip()
    values = {'at': "clock_timestamp()", 'user_id': "'"+user_id+"'",
              'token_hash': "decode('"+token_hash+"','hex')", 'idle_seconds': '1800',
              'created_at': "clock_timestamp()", 'expires_at': "(clock_timestamp()+interval '1 hour')"}
    sql = re.sub(r'sqlc.arg\((\w+)\)', lambda m: values[m[1]], sql)
    assert 'sqlc.' not in sql
    return sql
rotate = query('LoginRotateOldSession', old_hash)
assert rotate.endswith(';')
print(rotate[:-1] + ' RETURNING s.id AS rotated \\gset')
print(query('LoginInsertSession', new_hash))
print("SELECT 'LOGIN_ROTATED=1';")
PYSQL
}
read -r LOGIN_USER LOGIN_OLD_HASH LOGIN_NEW_A LOGIN_NEW_B <<<"$(sql <<'SQL' | tail -1
INSERT INTO app.users(email,password_hash) VALUES('rotation-race-'||uuidv7()||'@example.test','fixture-only') RETURNING id AS u \gset
INSERT INTO app.auth_sessions(user_id,token_hash,created_at,last_seen_at,expires_at)
 VALUES(:'u',sha256(uuidv7()::text::bytea),clock_timestamp()-interval '10 minutes',clock_timestamp()-interval '5 minutes',clock_timestamp()+interval '1 hour') RETURNING encode(token_hash,'hex') AS old \gset
SELECT :'u'||' '||:'old'||' '||encode(sha256(uuidv7()::text::bytea),'hex')||' '||encode(sha256(uuidv7()::text::bytea),'hex');
SQL
)"
LOGIN_FIRST_SQL=$(login_rotation_sql "$LOGIN_USER" "$LOGIN_OLD_HASH" "$LOGIN_NEW_A")
LOGIN_SECOND_SQL=$(login_rotation_sql "$LOGIN_USER" "$LOGIN_OLD_HASH" "$LOGIN_NEW_B")
gate_close 74
auth_tx login_rotate_first "$LOGIN_FIRST_SQL $(park 74)" &
login_first_pid=$!
await login_rotate_first advisory
auth_tx login_rotate_second "$LOGIN_SECOND_SQL" &
login_second_pid=$!
await login_rotate_second "${ROWLOCK[@]}"
blocked=$(sql <<<"SELECT first.pid=ANY(pg_blocking_pids(second.pid)) FROM pg_stat_activity first,pg_stat_activity second WHERE first.application_name='${RACE_PREFIX}login_rotate_first' AND second.application_name='${RACE_PREFIX}login_rotate_second';")
[[ $blocked == t ]] || { echo 'FAIL: login second did not block on old session'; fail=1; }
gate_open 74
wait "$login_first_pid" "$login_second_pid"
expect login_rotate_first ok; expect login_rotate_second 'no rows returned for'
result login_rotate_first 'LOGIN_ROTATED=1'
got=$(sql <<<"SELECT 'LOGIN_ROTATION_RACE='||count(*) FILTER(WHERE revoked_at IS NOT NULL)||'|'||count(*) FILTER(WHERE revoked_at IS NULL AND expires_at>clock_timestamp() AND last_seen_at>clock_timestamp()-interval '30 minutes')||'|'||count(*) FILTER(WHERE token_hash=decode('$LOGIN_NEW_A','hex'))||'|'||count(*) FILTER(WHERE token_hash=decode('$LOGIN_NEW_B','hex')) FROM app.auth_sessions WHERE user_id='$LOGIN_USER';")
echo "race conditional session rotation: $got (expected LOGIN_ROTATION_RACE=1|1|1|0)"
[[ $got == 'LOGIN_ROTATION_RACE=1|1|1|0' ]] || fail=1
# Notifications source dedup and same-key manual redrive use actual command
# functions on separate connections. Advisory gates 60+ reserve this module.
read -r NT NO <<<"$(sql <<'SQL' | tail -1
INSERT INTO app.tenants(name) VALUES('notifications-race') RETURNING id AS t \gset
SELECT set_config('app.tenant_id',:'t',false);
SET ROLE app_orders_executor;
INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
VALUES(:'t',uuidv7(),1,'race',sha256('race'::bytea),'{"email":"race@example.test"}','USD',100,0,0,'{}',100) RETURNING id AS oid \gset
RESET ROLE;
SELECT :'t'||' '||:'oid';
SQL
)"
source="INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES('$NT','order.paid',jsonb_build_object('order_id','$NO'::uuid),'orders.events','notification-race-source') ON CONFLICT(tenant_id,delivery_target,dedup_key) DO NOTHING;"
gate_close 60
tx notification_source_first app_orders_executor "$NT" "$source $(park 60)" &
await notification_source_first advisory
tx notification_source_second app_orders_executor "$NT" "$source" &
await notification_source_second "${ROWLOCK[@]}"
gate_open 60; wait
expect notification_source_first ok;expect notification_source_second ok
got=$(sql <<<"SELECT count(*)||'|'||(SELECT count(*) FROM app.outbox_events WHERE tenant_id='$NT' AND delivery_target='notifications.email') FROM app.notification_deliveries WHERE tenant_id='$NT';")
echo "notification concurrent source dedup: $got (expected 1|1)"
[[ $got == '1|1' ]] || fail=1
# A known failure witness is final-attempt failed, then real redrive races.
read -r ND NE <<<"$(sql <<SQL | tail -1
SELECT id AS d,dispatch_event_id AS e FROM app.notification_deliveries WHERE tenant_id='$NT' \gset
UPDATE app.notification_deliveries SET status='dead_letter',last_error='DELIVERY_REJECTED',attempt_count=10 WHERE tenant_id='$NT' AND id=:'d';
UPDATE app.outbox_events SET status='dead_letter',attempts=10 WHERE tenant_id='$NT' AND id=:'e';
SELECT :'d'||' '||:'e';
SQL
)"
redrive="SELECT 'redrive='||r.request_token||':'||r.generation FROM app.notification_redrive('$NT','$ND','race-redrive') r;"
gate_close 61
tx notification_redrive_first app_runtime "$NT" "$redrive $(park 61)" &
await notification_redrive_first advisory
tx notification_redrive_second app_runtime "$NT" "$redrive" &
await notification_redrive_second "${ROWLOCK[@]}"
gate_open 61;wait
expect notification_redrive_first ok;expect notification_redrive_second ok
receipt=$(sed -n 's/^redrive=//p' "$ERRDIR/notification_redrive_first.out")
[[ -n $receipt ]] || { echo 'FAIL: notification missing redrive receipt';fail=1; }
result notification_redrive_second "redrive=$receipt"
got=$(sql <<<"SELECT generation||'|'||status||'|'||(SELECT count(*) FROM app.notification_redrive_requests WHERE tenant_id='$NT')||'|'||(SELECT count(*) FROM app.outbox_events WHERE tenant_id='$NT' AND delivery_target='notifications.email') FROM app.notification_deliveries WHERE tenant_id='$NT' AND id='$ND';")
echo "notification concurrent redrive: $got (expected 2|pending|1|2)"
[[ $got == '2|pending|1|2' ]] || fail=1

# Quotes: freeze a pending quote and use the same expected version for both
# writers. Run both deterministic winners; a rowlock wait precedes gate release.
for winner in approve withdraw; do
 read -r T U A Q <<<"$(sql <<'SQL' | tail -1
INSERT INTO app.tenants(name) VALUES('quote-race') RETURNING id AS t \gset
INSERT INTO app.organizations(tenant_id,display_name) VALUES(:'t','R') RETURNING id AS o \gset
INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(:'t',:'o','R') RETURNING id AS s \gset
INSERT INTO app.users(email,password_hash) VALUES(uuidv7()::text||'@race.invalid','fixture') RETURNING id AS u \gset
INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(:'t',:'u','admin');
INSERT INTO app.customers(tenant_id,email) VALUES(:'t',uuidv7()::text||'@race.invalid') RETURNING id AS b \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'t',uuidv7()::text||'@race.invalid') RETURNING id AS a \gset
INSERT INTO app.buyer_companies(tenant_id,name) VALUES(:'t','R') RETURNING id AS c \gset
INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code)
VALUES(:'t',:'c',:'b','purchaser'),(:'t',:'c',:'a','approver');
INSERT INTO app.inquiries(tenant_id,site_id,buyer_email,message) VALUES(:'t',:'s','race@example.invalid','race') RETURNING id AS i \gset
BEGIN;
SELECT set_config('app.tenant_id',:'t',true);
SET LOCAL ROLE app_runtime;
SELECT app.create_quote(:'t',:'s',:'i',:'c',:'u','USD',100,0,0,100,'{}',clock_timestamp()+interval '1 hour',
 '[{"line_no":1,"description":"race","quantity":1,"unit_price":100,"line_total":100,"snapshot":{}}]') AS q \gset
SELECT app.transition_quote(:'t',:'q',1,'send','staff',:'u');
SELECT app.transition_quote(:'t',:'q',2,'accept','customer',:'b');
COMMIT;
SELECT :'t'||' '||:'u'||' '||:'a'||' '||:'q';
SQL
)"
 approve="SELECT app.transition_quote('$T','$Q',3,'approve','customer','$A');"
 withdraw="SELECT app.transition_quote('$T','$Q',3,'withdraw','staff','$U');"
 first=$approve; second=$withdraw; expected=approved; approvals=1
 if [[ $winner == withdraw ]]; then first=$withdraw; second=$approve; expected=withdrawn; approvals=0; fi
 gate_close 50
 tx quote_first app_runtime "$T" "$first $(park 50)" &
 await quote_first advisory
 tx quote_second app_runtime "$T" "$second" &
 await quote_second "${ROWLOCK[@]}"
 gate_open 50; wait
 expect quote_first ok; expect quote_second QUOTES_VERSION; expect quote_second P0003
 got=$(sql <<<"SELECT status||'|'||version||'|'||(SELECT count(*) FROM app.quote_approvals WHERE tenant_id='$T' AND quote_id='$Q')||'|'||(SELECT count(*) FROM app.outbox_events WHERE tenant_id='$T' AND payload->>'quote_id'='$Q') FROM app.quotes WHERE tenant_id='$T' AND id='$Q';")
 echo "quote $winner/contender race: $got (expected $expected|4|$approvals|4)"
 [[ $got == "$expected|4|$approvals|4" ]] || fail=1
done

# Checkout durable claim competes before provider IO. Both winner directions
# are synchronized by gates 80/81, with a witnessed contender row-lock wait.
# Then only the committed claimant applies Orders/Ledger side effects. Outbox:
# order.created + terminal event, plus the notifications fan-out email for paid. The Go
# pgx E2E repeats this protocol through the actual Fake provider/service.
for first_phase in confirming cancelling; do
 read -r T SKU OID <<<"$(inventory_fixture)"
 CID=$(sql <<<"SELECT uuidv7();")
 sql <<SQL >/dev/null
UPDATE app.tenants SET settlement_currency='USD' WHERE id='$T';
INSERT INTO app.ledger_accounts(tenant_id,code,currency,account_type) VALUES
 ('$T','cash','USD','asset'),('$T','accounts_receivable','USD','asset'),
 ('$T','revenue','USD','revenue'),('$T','tax_payable','USD','liability'),('$T','shipping_revenue','USD','revenue');
SELECT set_config('app.tenant_id','$T',false);
SET ROLE app_runtime;
SELECT (app.inventory_reserve('$SKU',1,'order','$OID','checkout-race-stock',900)).id AS rid \gset
SELECT app.orders_create('$OID','checkout-race-order',sha256('race'::bytea),NULL,'{"email":"race@example.invalid"}','USD',20,'{}',
 jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id','$SKU','sku','checkout-race','quantity',1,'unit_price_minor',100,
 'line_net_minor',100,'line_tax_minor',10,'line_total_minor',110,'reservation_id',:'rid','tax_snapshot','{}'::jsonb)),'[]');
INSERT INTO app.checkout_sessions(tenant_id,id,actor_kind,actor_id,request_key,request_hash,cart_id,cart_version,order_id,currency,
 amount_minor,items_net_minor,tax_minor,shipping_minor,payable_version,address_snapshot)
 VALUES('$T','$CID','guest',repeat('a',64),'checkout-race',sha256('start'::bytea),uuidv7(),1,'$OID','USD',130,100,10,20,1,'{}');
UPDATE app.checkout_sessions SET phase='ready',provider_ref='fake-race',start_result='{}' WHERE tenant_id='$T' AND id='$CID';
SQL
 if [[ $first_phase == confirming ]]; then second_phase=cancelling; first_kind=payment; second_kind=cancel; final=paid; stock=0; journals=1; outbox=3; race_gate=80
 else second_phase=confirming; first_kind=cancel; second_kind=payment; final=cancelled; stock=1; journals=0; outbox=2; race_gate=81; fi
 claim_first="WITH changed AS (UPDATE app.checkout_sessions SET phase='$first_phase',claim_kind='$first_kind',claim_key='race-first' WHERE tenant_id='$T' AND id='$CID' AND phase='ready' RETURNING id) SELECT 'C18_CLAIM_FIRST='||count(*) FROM changed;"
 claim_second="WITH changed AS (UPDATE app.checkout_sessions SET phase='$second_phase',claim_kind='$second_kind',claim_key='race-second' WHERE tenant_id='$T' AND id='$CID' AND phase='ready' RETURNING id) SELECT 'C18_CLAIM_SECOND='||count(*) FROM changed;"
 gate_close "$race_gate"
 tx checkout_first app_runtime "$T" "$claim_first $(park "$race_gate")" &
 await checkout_first advisory
 tx checkout_second app_runtime "$T" "$claim_second" &
 await checkout_second "${ROWLOCK[@]}"
 gate_open "$race_gate"; wait
 expect checkout_first ok; expect checkout_second ok
 result checkout_first 'C18_CLAIM_FIRST=1'; result checkout_second 'C18_CLAIM_SECOND=0'
 finish="SELECT app.orders_transition('$OID','$final','checkout-race-terminal',0);"
 if [[ $final == paid ]]; then
 finish+=" SELECT app.post_ledger('$T','USD','order','$OID','checkout:race',
 ARRAY[(SELECT id FROM app.ledger_accounts WHERE tenant_id='$T' AND code='accounts_receivable'),(SELECT id FROM app.ledger_accounts WHERE tenant_id='$T' AND code='revenue'),(SELECT id FROM app.ledger_accounts WHERE tenant_id='$T' AND code='tax_payable'),(SELECT id FROM app.ledger_accounts WHERE tenant_id='$T' AND code='shipping_revenue'),(SELECT id FROM app.ledger_accounts WHERE tenant_id='$T' AND code='cash'),(SELECT id FROM app.ledger_accounts WHERE tenant_id='$T' AND code='accounts_receivable')], ARRAY['debit','credit','credit','credit','debit','credit'],ARRAY[130,100,10,20,130,130]::bigint[]);"
 fi
 tx checkout_finish app_runtime "$T" "$finish UPDATE app.checkout_sessions SET phase='$final',final_result='{}' WHERE tenant_id='$T' AND id='$CID';"
 expect checkout_finish ok
 got=$(sql <<<"SELECT c.phase||'|'||o.state||'|'||s.on_hand||'|'||s.reserved||'|'||(SELECT count(*) FROM app.ledger_transactions WHERE tenant_id='$T')||'|'||(SELECT count(*) FROM app.outbox_events WHERE tenant_id='$T')||'|'||(SELECT count(*) FROM app.inventory_movements WHERE tenant_id='$T') FROM app.checkout_sessions c JOIN app.orders o ON(o.tenant_id,o.id)=(c.tenant_id,c.order_id) JOIN app.stock_levels s ON s.tenant_id=c.tenant_id WHERE c.tenant_id='$T' AND c.id='$CID';")
 echo "checkout claim $first_phase wins: $got (expected $final|$final|$stock|0|$journals|$outbox|3)"
 [[ $got == "$final|$final|$stock|0|$journals|$outbox|3" ]] || fail=1
done

if [[ $fail -ne 0 ]]; then echo "FAIL: race end state differs from the sealing/reliability/inventory/pricing/shipping/tax/customers/ledger/catalog/cart/orders/notifications/quotes/checkout contract" >&2; exit 1; fi
echo 'PASS: lock-synchronized sealing, disjoint outbox claims, lease fencing and same-key occupancy'
echo 'PASS: inventory last-unit/key-replay/expiry-commit races preserve quantity and movement counts'
echo 'PASS: pricing duplicate-tier race keeps one committed tier'
echo 'PASS: shipping range INSERT/UPDATE races preserve exact non-overlapping tiers'
echo 'PASS: tax INSERT/UPDATE exclusion races preserve non-overlapping windows'
echo 'PASS: customer normalized email registrations commit once per tenant'
echo 'PASS: ledger identical and conflicting posting races preserve one posted journal/two entries'
echo 'PASS: catalog stale CAS waits for row lock, rechecks version and leaves winner unchanged'
echo 'PASS: cart row-lock/CAS contender cannot overwrite winner line or version'
echo 'PASS: orders pay/cancel and prelock/expiry races conserve inventory and events'
echo 'PASS: quote approve/withdraw row-lock CAS races have exactly one winner and atomic outbox/approval'
echo 'PASS: checkout confirm/cancel claims choose one durable winner with atomic order, stock, ledger and outbox witnesses'
# Focused native River/claim/fence gates cannot silently skip for missing PG.
reliability_packages=(./internal/db ./internal/platform/jobs ./internal/platform/idempotency ./internal/platform/outbox)
if [[ -n "${DATABASE_URL:-}" ]]; then
 JOBS_REQUIRE_PG=1 IDEMPOTENCY_REQUIRE_PG=1 OUTBOX_REQUIRE_PG=1 GOTOOLCHAIN=local \
  go test -race -tags pgx -count=1 -timeout=120s -run '^(TestPG|TestJobsPG)' "${reliability_packages[@]}"
else
 # Compose has no host port. Test binaries run against its loopback database;
 # the external/CI path above also runs the race detector.
 (
  compose=(docker compose -f deploy/compose.yaml)
  arch="$("${compose[@]}" exec -T postgres uname -m)"
  case "$arch" in aarch64|arm64) arch=arm64 ;; x86_64) arch=amd64 ;; *) echo 'Unsupported test architecture' >&2; exit 4 ;; esac
  temp="$(mktemp -d "${TMPDIR:-/tmp}/opensite-reliability.XXXXXX")"
  container_bin="/tmp/opensite-reliability-$(basename "$temp")"
  cleanup_go() { "${compose[@]}" exec -T postgres rm -f "$container_bin" >/dev/null 2>&1 || true; rm -rf "$temp"; }
  trap cleanup_go EXIT
  for package in "${reliability_packages[@]}"; do
   GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -tags pgx -o "$temp/gate" "$package"
   "${compose[@]}" cp "$temp/gate" "postgres:$container_bin"
   "${compose[@]}" exec -T -e DATABASE_URL=postgresql://postgres@localhost/opensite?sslmode=disable \
    -e JOBS_REQUIRE_PG=1 -e IDEMPOTENCY_REQUIRE_PG=1 -e OUTBOX_REQUIRE_PG=1 postgres \
    sh -c 'export PGPASSWORD="$POSTGRES_PASSWORD"; exec "$@"' sh "$container_bin" \
    '-test.run=^(TestPG|TestJobsPG)' -test.count=1 -test.timeout=120s -test.v
  done
 )
fi
echo 'PASS: native River transaction/admission/restart and Go idempotency/outbox PG gates'
echo 'PASS: auth concurrent normalized-identifier failures preserve account and account+IP counts'
echo 'PASS: bootstrap concurrent first-owner creation commits one installation owner only'
echo 'PASS: auth cleanup skips locked stale rows and preserves concurrent renewals'
echo 'PASS: conditional login rotation consumes one old session and commits exactly one successor'
