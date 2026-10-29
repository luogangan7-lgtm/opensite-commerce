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
# shellcheck source=scripts/lib/pg_url.sh
source "$ROOT/scripts/lib/pg_url.sh"
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
claim_a="INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,claim_token) VALUES('$T','service','$WA','race','ab46d1fc-b24a-5c82-b0c1-f4a132d258d0',sha256('same-request'::bytea),clock_timestamp()+interval '1 hour','$IA') ON CONFLICT (tenant_id,actor_kind,actor_id,operation,idempotency_key) DO UPDATE SET idempotency_key=EXCLUDED.idempotency_key RETURNING 'acquired='||(claim_token='$IA'::uuid)||'|'||state||'|'||encode(request_hash,'hex');"
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
sql <<<"INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,claim_token) VALUES('$T','service','$WA','reclaim','9fc71cc5-dcdd-5a3f-9e4d-15742b8f3271',sha256('r'::bytea),clock_timestamp()-interval '1 second','$IO');" >/dev/null
reclaim_a="SELECT 'owner='||(claim_token='$IA'::uuid) FROM app.idempotency_claim('$T','service','$WA','reclaim','9fc71cc5-dcdd-5a3f-9e4d-15742b8f3271',sha256('r'::bytea),interval '1 hour','$IA');"
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
# Fixture capabilities are independent CSPRNG bytes; the same key shares one bearer.
inventory_bearer() { od -An -N32 -tx1 /dev/urandom | tr -d ' \n'; }
read -r T SKU OWNER <<<"$(inventory_fixture)"
HOLDER=$(inventory_bearer)
gate_close 11
tx inv_first app_runtime "$T" "SELECT app.inventory_reserve_non_order('$SKU',1,'checkout','$OWNER','79d596c6-b639-58c9-89bf-d295a401d63f',900,decode('$HOLDER','hex')); $(park 11)" &
await inv_first advisory
tx inv_second app_runtime "$T" "SELECT app.inventory_reserve_non_order('$SKU',1,'checkout','$OWNER','6791e958-33e7-589b-8191-e7a1048d81b1',900,decode('$HOLDER','hex'));" &
await inv_second "${ROWLOCK[@]}"
gate_open 11; wait
expect inv_first ok; expect inv_second INVENTORY_INSUFFICIENT_STOCK; expect inv_second P0001
inventory_check 'race inventory last unit' '1|1|1|2' "$T" "$SKU"

for quantity in 1 2; do
  read -r T SKU OWNER <<<"$(inventory_fixture)"
  HOLDER=$(inventory_bearer)
  gate_close 12
  tx inv_key_first app_runtime "$T" "SELECT app.inventory_reserve_non_order('$SKU',1,'checkout','$OWNER','ab46d1fc-b24a-5c82-b0c1-f4a132d258d0',900,decode('$HOLDER','hex')); $(park 12)" &
  await inv_key_first advisory
  tx inv_key_second app_runtime "$T" "SELECT app.inventory_reserve_non_order('$SKU',$quantity,'checkout','$OWNER','ab46d1fc-b24a-5c82-b0c1-f4a132d258d0',900,decode('$HOLDER','hex'));" &
  await inv_key_second advisory
  gate_open 12; wait
  expect inv_key_first ok
  if [[ $quantity == 1 ]]; then expect inv_key_second ok
  else expect inv_key_second INVENTORY_IDEMPOTENCY_KEY_REUSED; expect inv_key_second 22023; fi
  inventory_check "race inventory same-key qty=$quantity" '1|1|1|2' "$T" "$SKU"
done

read -r T SKU OWNER <<<"$(inventory_fixture)"
RID=$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SELECT (app.inventory_reserve('$SKU',1,'order','$OWNER','babcc8b2-ad84-5936-a1b1-4de223d23209',900)).id;" | tail -1)
sql <<<"UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id='$T' AND id='$RID';" >/dev/null
gate_close 13
tx inv_expire app_queue_worker "$T" "SELECT app.inventory_expire_due(1); $(park 13)" &
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
RID=$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SELECT (app.inventory_reserve('$SKU',1,'order','$OWNER','71f15804-7627-5bf5-a07b-46365a809760',900)).id;" | tail -1)
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
SKIPPED=$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SET ROLE app_queue_worker; SELECT app.inventory_expire_due(1);" | tail -1)
echo "race inventory expiry skips busy commit: $SKIPPED (expected 0)"
[[ $SKIPPED == 0 ]] || fail=1
gate_open 14; wait
expect inv_stock ok; expect inv_commit_wait INVENTORY_RESERVATION_EXPIRED; expect inv_commit_wait 55000
sql <<<"SELECT set_config('app.tenant_id','$T',false); SET ROLE app_queue_worker; SELECT app.inventory_expire_due(1);" >/dev/null
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
HOLDER=$(inventory_bearer)
R1=$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SET ROLE app_runtime; SELECT (app.inventory_reserve_non_order('$SKU',1,'checkout','$OWNER','00daed1d-92a2-5865-acc8-323483ba1dd8',1,decode('$HOLDER','hex'))).id;" | tail -1)
R2=$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SET ROLE app_runtime; SELECT (app.inventory_reserve_non_order('$SKU',1,'checkout','$OWNER','29b0d525-7eab-5fc2-bc4d-c65aed42e6dd',2,decode('$HOLDER','hex'))).id;" | tail -1)
for _ in $(seq 1 100); do  # poll the database clock until both are due; never order actors by sleep
  [[ $(sql <<<"SELECT count(*) FROM app.inventory_reservations WHERE tenant_id='$T' AND expires_at<=clock_timestamp();") == 2 ]] && break
  sleep 0.05
done
gate_close 16
tx sweep_hold postgres "$T" "SELECT 1 FROM app.inventory_reservations WHERE id='$R1' FOR UPDATE; $(park 16)" &
await sweep_hold advisory
SWEPT=$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SET ROLE app_queue_worker; SELECT app.inventory_expire_due(1);" | tail -1)
gate_open 16; wait
echo "race expiry sweep skips locked oldest: expired=$SWEPT (expected 1)"
[[ $SWEPT == 1 ]] || fail=1
got=$(sql <<<"SELECT string_agg(state,',' ORDER BY expires_at) FROM app.inventory_reservations WHERE tenant_id='$T';")
echo "race expiry sweep states: $got (expected active,expired)"
[[ $got == 'active,expired' ]] || fail=1
# C-20b: public observation of the tuple/hash never grants terminal authority.
# The legitimate command parks with the row locked and its state uncommitted;
# a foreign replay must wait for that lock, then reject even a terminal replay.
for terminal in release commit; do
 for attack in observed_hash wrong_bearer; do
  read -r T SKU OWNER <<<"$(inventory_fixture)"
  HOLDER=$(inventory_bearer)
  RID=$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SET ROLE app_runtime; SELECT (app.inventory_reserve_non_order('$SKU',1,'checkout','$OWNER','ac791813-a8fa-5264-9df6-f1b6ce69b5b8',900,decode('$HOLDER','hex'))).id;" | tail -1)
  read -r OBSERVED_OWNER OBSERVED_HASH <<<"$(sql <<<"SELECT set_config('app.tenant_id','$T',false); SET ROLE app_runtime; SELECT owner_id||' '||encode(holder_hash,'hex') FROM app.inventory_reservations WHERE id='$RID';" | tail -1)"
  [[ $OBSERVED_OWNER == "$OWNER" && ${#OBSERVED_HASH} == 64 ]] || { echo 'FAIL: holder race lacks runtime observation'; exit 1; }
  FOREIGN_BEARER=$OBSERVED_HASH
  [[ $attack != wrong_bearer ]] || FOREIGN_BEARER=$(inventory_bearer)
  gate_close 140
  tx inv_holder app_runtime "$T" "SELECT app.inventory_${terminal}_non_order('$RID','checkout','$OWNER',decode('$HOLDER','hex')); $(park 140)" &
  await inv_holder advisory
  tx inv_foreign app_runtime "$T" "SELECT app.inventory_${terminal}_non_order('$RID','checkout','$OBSERVED_OWNER',decode('$FOREIGN_BEARER','hex'));" &
  await inv_foreign "${ROWLOCK[@]}"
  BLOCKED=$(sql <<<"SELECT count(*)=1 AND bool_and(h.pid=ANY(pg_blocking_pids(f.pid))) FROM pg_stat_activity f CROSS JOIN pg_stat_activity h WHERE f.application_name='${RACE_PREFIX}inv_foreign' AND h.application_name='${RACE_PREFIX}inv_holder';")
  [[ $BLOCKED == t ]] || { echo 'FAIL: foreign finish did not wait on holder reservation'; fail=1; }
  gate_open 140; wait
  expect inv_holder ok; expect inv_foreign INVENTORY_HOLDER_MISMATCH; expect inv_foreign 42501
  tx inv_holder_replay app_runtime "$T" "SELECT app.inventory_${terminal}_non_order('$RID','checkout','$OWNER',decode('$HOLDER','hex'));"
  expect inv_holder_replay ok
  physical=1; state=released
  if [[ $terminal == commit ]]; then physical=0; state=committed; fi
  inventory_check "race inventory holder $terminal / $attack" "$physical|0|0|3" "$T" "$SKU"
  got=$(sql <<<"SELECT r.state||'|'||(SELECT count(*) FROM app.inventory_movements m WHERE m.reservation_id=r.id AND m.kind='$terminal') FROM app.inventory_reservations r WHERE r.id='$RID';")
  echo "race inventory holder terminal witness: $got (expected $state|1)"
  [[ $got == "$state|1" ]] || fail=1
 done
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
  got=$(sql <<<"SELECT count(*)||'|'||(SELECT count(*) FROM app.ledger_entries WHERE tenant_id='$T')||'|'||bool_and(status='posted' AND posted_at IS NOT NULL)||'|'||(SELECT sum(amount) FILTER(WHERE side='debit')||'|'||sum(amount) FILTER(WHERE side='credit') FROM app.ledger_entries WHERE tenant_id='$T') FROM app.ledger_transactions WHERE tenant_id='$T';")
  echo "ledger race $isolation/$scenario: $got (expected 1|2|true|100|100)"
  [[ $got == '1|2|true|100|100' ]] || fail=1
 done
done

# LEDGER-ISOLATION-GATE #7, gate140: INSERT holds the parent while posting
# establishes its snapshot. Only fixture construction disables the deferred
# header check, to model an existing draft unavailable through the runtime API.
# Production guards, roles, deferred checks and the revision fence remain active
# for every participant. Polling observes locks; no sleep determines ordering.
for isolation in 'READ COMMITTED' 'REPEATABLE READ' 'SERIALIZABLE'; do
 read -r T J A B <<<"$(sql <<'SQL' | tail -1
BEGIN;
INSERT INTO app.tenants(name,settlement_currency) VALUES('ledger-snapshot','USD') RETURNING id AS t \gset
SELECT set_config('app.tenant_id',:'t',true);
INSERT INTO app.ledger_accounts(tenant_id,code,account_type,currency)
 VALUES(:'t','cash','asset','USD') RETURNING id AS a \gset
INSERT INTO app.ledger_accounts(tenant_id,code,account_type,currency)
 VALUES(:'t','revenue','revenue','USD') RETURNING id AS b \gset
ALTER TABLE app.ledger_transactions DISABLE TRIGGER ledger_final_balance;
INSERT INTO app.ledger_transactions(tenant_id,currency,source_type,source_id,event)
 VALUES(:'t','USD','race','entry-v-posting','capture') RETURNING id AS j \gset
INSERT INTO app.ledger_entries(tenant_id,transaction_id,line_no,account_id,currency,side,amount)
 VALUES(:'t',:'j',1,:'a','USD','debit',100),(:'t',:'j',2,:'b','USD','credit',100);
ALTER TABLE app.ledger_transactions ENABLE TRIGGER ledger_final_balance;
COMMIT;
SELECT :'t'||' '||:'j'||' '||:'a'||' '||:'b';
SQL
)"
 insert="INSERT INTO app.ledger_entries(tenant_id,transaction_id,line_no,account_id,currency,side,amount) VALUES('$T','$J',3,'$A','USD','debit',1);"
 posting="SELECT 'LEDGER_SNAPSHOT='||count(*) FROM app.ledger_entries WHERE tenant_id='$T' AND transaction_id='$J'; UPDATE app.ledger_transactions SET status='posted',posted_at=clock_timestamp() WHERE tenant_id='$T' AND id='$J';"
 gate_close 140
 tx ledger_entry_insert app_ledger_executor "$T" "$insert $(park 140)" & entry_pid=$!
 await ledger_entry_insert advisory
 tx ledger_snapshot_post app_ledger_executor "$T" "$posting" "SET default_transaction_isolation='$isolation';" & post_pid=$!
 await ledger_snapshot_post "${ROWLOCK[@]}"
 gate_open 140
 await_job ledger_entry_insert "$entry_pid"; await_job ledger_snapshot_post "$post_pid"
 expect ledger_entry_insert ok
 result ledger_snapshot_post 'LEDGER_SNAPSHOT=2'
 if [[ $isolation == 'READ COMMITTED' ]]; then
  expect ledger_snapshot_post LEDGER_FINAL_BALANCE
 else
  expect ledger_snapshot_post 40001
 fi
 # A fresh BEGIN repeats the complete command under the original isolation.
 # Its snapshot must now contain the INSERT and reject the unbalanced posting.
 tx ledger_snapshot_retry app_ledger_executor "$T" "$posting" "SET default_transaction_isolation='$isolation';"
 result ledger_snapshot_retry 'LEDGER_SNAPSHOT=3'
 expect ledger_snapshot_retry LEDGER_FINAL_BALANCE
 got=$(sql <<<"SELECT status||'|'||revision||'|'||(SELECT sum(amount) FILTER(WHERE side='debit')||'|'||sum(amount) FILTER(WHERE side='credit') FROM app.ledger_entries WHERE tenant_id='$T' AND transaction_id='$J') FROM app.ledger_transactions WHERE tenant_id='$T' AND id='$J';")
 [[ $got == 'posting|3|101|100' ]] || { echo "FAIL LEDGER_SNAPSHOT_FENCE $isolation: $got"; fail=1; }
 # Complete the draft using a new balanced command, then witness committed sums
 # independently of the posting transaction's snapshot, including account nets.
 tx ledger_snapshot_balance app_ledger_executor "$T" "INSERT INTO app.ledger_entries(tenant_id,transaction_id,line_no,account_id,currency,side,amount) VALUES('$T','$J',4,'$B','USD','credit',1); $posting" "SET default_transaction_isolation='$isolation';"
 expect ledger_snapshot_balance ok
 got=$(sql <<<"SELECT j.status||'|'||j.revision||'|'||count(*)||'|'||sum(e.amount) FILTER(WHERE e.side='debit')||'|'||sum(e.amount) FILTER(WHERE e.side='credit')||'|'||sum(CASE WHEN e.account_id='$A' THEN CASE e.side WHEN 'debit' THEN e.amount ELSE -e.amount END ELSE 0 END)||'|'||sum(CASE WHEN e.account_id='$B' THEN CASE e.side WHEN 'credit' THEN e.amount ELSE -e.amount END ELSE 0 END) FROM app.ledger_transactions j JOIN app.ledger_entries e ON(e.tenant_id,e.transaction_id)=(j.tenant_id,j.id) WHERE j.tenant_id='$T' AND j.id='$J' GROUP BY j.status,j.revision;")
 [[ $got == 'posted|4|4|101|101|101|101' ]] || { echo "FAIL LEDGER_FINAL_BALANCES $isolation: $got"; fail=1; }
 echo "LEDGER gate140 $isolation final witness: $got"
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
SELECT (app.inventory_reserve('$SKU',1,'order','$OID','775f14e4-9898-57e2-9f9e-5069ddbea7fe',900)).id AS rid \gset
SELECT app.orders_create('$OID','9e668d72-afe6-5f1f-8392-143cc9c202c8',sha256('race'::bytea),NULL,
 '{"email":"race@example.invalid"}','USD',20,'{"amount_minor":20,"currency":"USD","service":"fixture"}',
 jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id','$SKU','sku','race-sku','quantity',1,
 'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',10,'line_total_minor',110,
 'reservation_id',:'rid','tax_snapshot',jsonb_build_object('category','standard','net_minor',100,'tax_minor',10,'gross_minor',110,'currency','USD'))),'[]');
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
SELECT (app.inventory_reserve('$SKU',1,'order','$OID','c5f53b7e-1ee3-5141-865b-35e8516aab36',900)).id AS r1 \gset
SELECT (app.inventory_reserve('$SKU',1,'order','$OID','087abf2c-eece-5179-8ac2-bb18b11a239e',900)).id AS r2 \gset
SELECT app.orders_create('$OID','db3e7e31-2167-586e-9102-cf83ec39583e',sha256('prelock'::bytea),NULL,
 '{"email":"prelock@example.invalid"}','USD',0,'{}',
 jsonb_build_array(
  jsonb_build_object('id',uuidv7(),'sku_id','$SKU','sku','prelock-sku','quantity',1,
   'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,
   'reservation_id',:'r1','tax_snapshot','{"category":"standard"}'::jsonb),
  jsonb_build_object('id',uuidv7(),'sku_id','$SKU','sku','prelock-sku','quantity',1,
   'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,
   'reservation_id',:'r2','tax_snapshot','{"category":"standard"}'::jsonb)), '[]');
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
 tx orders_prelock_sweep app_queue_worker "$T" "SELECT 'skipped='||app.inventory_expire_due(100);"
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
tx orders_prelock_cleanup app_queue_worker "$T" "SELECT 'expired='||app.inventory_expire_due(100);"
expect orders_prelock_cleanup ok; result orders_prelock_cleanup 'expired=1'
tx orders_prelock_cancel app_runtime "$T" "SELECT app.orders_transition('$OID','cancelled','prelock-cancel',0);"
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
 bootstrap_url="$(pg_url_for_database "$DATABASE_URL" "$BOOTSTRAP_DB")"
 PSQL=(psql "$bootstrap_url" -X -q -At -v ON_ERROR_STOP=1 -v VERBOSITY=verbose)
else
 PSQL=(docker compose -f deploy/compose.yaml exec -T postgres psql -X -q -At -U postgres -d "$BOOTSTRAP_DB" -v ON_ERROR_STOP=1 -v VERBOSITY=verbose)
fi
actual_db=$(sql -c 'SELECT current_database()')
[[ $actual_db == "$BOOTSTRAP_DB" ]] || { echo "FAIL: bootstrap target is $actual_db, not $BOOTSTRAP_DB (dbname override)" >&2; exit 1; }
for f in migrations/[0-9][0-9][0-9]_*.sql; do
 # Queue authorization depends on River's schema in this fresh database too.
 if [[ $f == *_queue_authorization.sql ]]; then
  if [[ -n "${DATABASE_URL:-}" ]]; then
   DATABASE_URL="$bootstrap_url" ./scripts/migrate_river.sh
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


# H4-clock: failure time is taken after the witnessed advisory wait, within
# production 500ms lock / 1500ms statement budgets (no sleep ordering).
CLOCK_IDENTIFIER="${RACE_PREFIX}clock@example.test"
gate_close 75
tx clock_holder postgres "$T" "SELECT pg_advisory_xact_lock(hashtextextended('opensite:auth:$CLOCK_IDENTIFIER',0)); $(park 75)" &
clock_holder_pid=$!
await clock_holder advisory
auth_tx clock_waiter "SET LOCAL lock_timeout='500ms'; SET LOCAL statement_timeout='1500ms'; SELECT app.auth_record_failure('$CLOCK_IDENTIFIER','192.0.2.75');" &
clock_waiter_pid=$!
await clock_waiter advisory
blocked=$(sql <<<"SELECT h.pid=ANY(pg_blocking_pids(w.pid)) FROM pg_stat_activity h,pg_stat_activity w WHERE h.application_name='${RACE_PREFIX}clock_holder' AND w.application_name='${RACE_PREFIX}clock_waiter';")
[[ $blocked == t ]] || { echo 'FAIL: clock waiter not blocked on holder'; fail=1; }
clock_release=$(sql <<<"SELECT clock_timestamp();")
gate_open 75
wait "$clock_holder_pid" "$clock_waiter_pid"
expect clock_holder ok; expect clock_waiter ok
got=$(sql <<<"SELECT cooldown_until >= '$clock_release'::timestamptz+interval '1 second' AND last_failed_at >= '$clock_release'::timestamptz FROM app.auth_account_attempts WHERE identifier_normalized='$CLOCK_IDENTIFIER';")
[[ $got == t ]] || { echo 'FAIL: post-wait DB clock lost full backoff'; fail=1; }

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
              'verified_password_hash': "'fixture-only'",
              'created_at': "clock_timestamp()", 'expires_at': "(clock_timestamp()+interval '1 hour')"}
    sql = re.sub(r'sqlc.arg\((\w+)\)', lambda m: values[m[1]], sql)
    assert 'sqlc.' not in sql
    return sql
print(query('LoginLockUser', old_hash).rstrip(';') + ' \\gset')
print(query('LoginLockSession', old_hash).rstrip(';') + ' \\gset')
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
auth_tx login_rotate_first "SET LOCAL lock_timeout='500ms'; SET LOCAL statement_timeout='1500ms'; $LOGIN_FIRST_SQL $(park 74)" &
login_first_pid=$!
await login_rotate_first advisory
auth_tx login_rotate_second "SET LOCAL lock_timeout='500ms'; SET LOCAL statement_timeout='1500ms'; $LOGIN_SECOND_SQL" &
login_second_pid=$!
await login_rotate_second "${ROWLOCK[@]}"
blocked=$(sql <<<"SELECT first.pid=ANY(pg_blocking_pids(second.pid)) FROM pg_stat_activity first,pg_stat_activity second WHERE first.application_name='${RACE_PREFIX}login_rotate_first' AND second.application_name='${RACE_PREFIX}login_rotate_second';")
[[ $blocked == t ]] || { echo 'FAIL: login second did not block on user-before-session lock'; fail=1; }
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
VALUES(:'t',uuidv7(),1,'da6dab07-57d2-550c-b107-3a405eccb15e',sha256('race'::bytea),'{"email":"race@example.test"}','USD',100,0,0,'{}',100) RETURNING id AS oid \gset
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
redrive="SELECT 'redrive='||r.request_token||':'||r.generation FROM app.notification_redrive('$NT','$ND','dd0c3f13-4fa9-5a1b-80c4-98f5ba228b58') r;"
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

# C-22a data rights races BEGIN: controlled capability calls hold customer then
# request locks. Gates80–86 are reserved here; pg_stat_activity witnesses every
# contender's wait before release, so elapsed time never chooses the winner.
privacy_fixture() {
 sql <<'SQL' | tail -1
INSERT INTO app.tenants(name) VALUES('privacy-race-synthetic') RETURNING id AS t \gset
INSERT INTO app.customers(tenant_id,email,display_name) VALUES(:'t','race@example.invalid','Synthetic race') RETURNING id AS c \gset
SELECT uuidv7() AS request,uuidv7() AS actor \gset
BEGIN;
SELECT set_config('app.tenant_id',:'t',true);
SET LOCAL ROLE app_runtime;
SELECT app.data_rights_request_export(:'request',:'c','service',:'actor');
COMMIT;
SELECT :'t'||' '||:'c'||' '||:'request'||' '||:'actor';
SQL
}
read -r T CUSTOMER REQUEST ACTOR <<<"$(privacy_fixture)"
tx privacy_ready app_runtime "$T" "SELECT app.data_rights_build_export('$REQUEST'); SELECT app.data_rights_complete_export('$REQUEST',sha256('privacy-race-token'::bytea),sha256('privacy-race-content'::bytea),app.data_rights_download_deadline('$REQUEST'));"
expect privacy_ready ok
gate_close 80
tx privacy_download_first app_runtime "$T" "SELECT 'download='||(app.data_rights_consume_export('$REQUEST','$CUSTOMER',sha256('privacy-race-token'::bytea))->>'id'); $(park 80)" &
await privacy_download_first advisory
tx privacy_download_second app_runtime "$T" "SELECT app.data_rights_consume_export('$REQUEST','$CUSTOMER',sha256('privacy-race-token'::bytea));" &
await privacy_download_second "${ROWLOCK[@]}"
gate_open 80; wait
expect privacy_download_first ok; expect privacy_download_second 55000
result privacy_download_first "download=$REQUEST"
got=$(sql <<<"SELECT status||'|'||(token_hash=sha256('privacy-race-token'::bytea)) FROM app.data_rights_exports WHERE tenant_id='$T' AND id='$REQUEST';")
echo "privacy concurrent single-use download: $got (expected consumed|true)"
[[ $got == 'consumed|true' ]] || fail=1

# Worker starts building and owns the customer lock before erasure is admitted;
# completion commits first, then erasure revokes it and scrubs both hashes.
read -r T CUSTOMER REQUEST ACTOR <<<"$(privacy_fixture)"
gate_close 81
tx privacy_build_first app_runtime "$T" "SELECT app.data_rights_build_export('$REQUEST'); $(park 81) SELECT app.data_rights_complete_export('$REQUEST',sha256('privacy-race-token'::bytea),sha256('privacy-race-content'::bytea),app.data_rights_download_deadline('$REQUEST'));" &
await privacy_build_first advisory
# Prove the worker owns the CUSTOMER lock specifically, not merely a request
# row that would make erasure wait later in its lock sequence.
tx privacy_customer_lock_probe postgres "$T" "DO \$proof\$ BEGIN
 BEGIN PERFORM id FROM app.customers WHERE tenant_id='$T' AND id='$CUSTOMER' FOR UPDATE NOWAIT;
 RAISE EXCEPTION 'PRIVACY_CUSTOMER_LOCK_GATE missing customer lock before erasure';
 EXCEPTION WHEN lock_not_available THEN NULL; END;
 END \$proof\$;"
expect privacy_customer_lock_probe ok
tx privacy_erase_second app_runtime "$T" "SELECT app.data_rights_erase_customer('$CUSTOMER','service','$ACTOR');" &
await privacy_erase_second "${ROWLOCK[@]}"
gate_open 81; wait
expect privacy_build_first ok; expect privacy_erase_second ok
got=$(sql <<<"SELECT c.status||'|'||r.status||'|'||(r.token_hash IS NULL AND r.content_hash IS NULL)||'|'||(SELECT count(*) FROM app.data_rights_erasures WHERE tenant_id='$T' AND customer_id='$CUSTOMER') FROM app.customers c JOIN app.data_rights_exports r ON (r.tenant_id,r.customer_id)=(c.tenant_id,c.id) WHERE c.tenant_id='$T' AND c.id='$CUSTOMER' AND r.id='$REQUEST';")
echo "privacy completion followed by erasure: $got (expected disabled|revoked|true|1)"
[[ $got == 'disabled|revoked|true|1' ]] || fail=1
for operation in build download consume complete; do
 case $operation in
  build) call="app.data_rights_build_export('$REQUEST')" ;;
  download) call="app.data_rights_download_info('$REQUEST','$CUSTOMER')" ;;
  consume) call="app.data_rights_consume_export('$REQUEST','$CUSTOMER',sha256('privacy-race-token'::bytea))" ;;
  complete) call="app.data_rights_complete_export('$REQUEST',sha256('privacy-race-token'::bytea),sha256('privacy-race-content'::bytea),clock_timestamp()+interval '1 minute')" ;;
 esac
 tx "privacy_after_erase_$operation" app_runtime "$T" "SELECT $call;"
 expect "privacy_after_erase_$operation" 55000
done

# Reverse admission proves a queued worker cannot rebuild after erasure commits.
read -r T CUSTOMER REQUEST ACTOR <<<"$(privacy_fixture)"
gate_close 82
tx privacy_erase_first app_runtime "$T" "SELECT app.data_rights_erase_customer('$CUSTOMER','service','$ACTOR'); $(park 82)" &
await privacy_erase_first advisory
tx privacy_build_second app_runtime "$T" "SELECT app.data_rights_build_export('$REQUEST');" &
await privacy_build_second "${ROWLOCK[@]}"
gate_open 82; wait
expect privacy_erase_first ok; expect privacy_build_second 55000
# C22a notification erasure gates83–86: 84 uses two extra barrier phases.
privacy_notification_fixture() {
 sql <<'SQL' | tail -1
INSERT INTO app.tenants(name) VALUES('privacy-notification-race') RETURNING id AS t \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'t','privacy-notify@example.invalid') RETURNING id AS c \gset
SELECT uuidv7() AS oid,uuidv7() AS request,uuidv7() AS actor \gset
BEGIN;
SELECT set_config('app.tenant_id',:'t',true);
SET LOCAL ROLE app_orders_executor;
INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,customer_id,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
 VALUES(:'t',:'oid',1,'da685aff-cbea-534d-9c18-be779b73a3f4',sha256('race'::bytea),:'c','USD',100,0,0,'{"amount_minor":0,"currency":"USD","address":"Synthetic private street"}',100);
RESET ROLE;
INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
 VALUES(:'t','order.paid',jsonb_build_object('order_id',:'oid'),'orders.events','privacy-notification-race') RETURNING id AS source \gset
SELECT id AS notification,dispatch_event_id AS dispatch FROM app.notification_deliveries WHERE tenant_id=:'t' AND source_event_id=:'source' \gset
UPDATE app.notification_deliveries SET status='dead_letter',last_error='DELIVERY_REJECTED' WHERE tenant_id=:'t' AND id=:'notification';
UPDATE app.outbox_events SET status='dead_letter',attempts=10 WHERE tenant_id=:'t' AND id=:'dispatch';
SET LOCAL ROLE app_runtime;
SELECT app.data_rights_request_export(:'request',:'c','service',:'actor');
COMMIT;
SELECT :'t'||' '||:'c'||' '||:'oid'||' '||:'notification'||' '||:'request'||' '||:'actor';
SQL
}
# Redrive owns the envelope but its new dispatch is not yet visible. Erasure
# cannot wait holding older dispatches; NOWAIT must roll everything back.
read -r T CUSTOMER PRIVACY_ORDER NOTIFICATION REQUEST ACTOR <<<"$(privacy_notification_fixture)"
gate_close 83
tx privacy_redrive_hold app_runtime "$T" "SELECT app.notification_redrive('$T','$NOTIFICATION','a20ac528-d689-5fae-868d-0dda9ba3aba2'); $(park 83)" &
await privacy_redrive_hold advisory
tx privacy_redrive_erase_busy app_runtime "$T" "SELECT app.data_rights_erase_customer('$CUSTOMER','service','$ACTOR');" &
PRIVACY_BUSY_PID=$!
await_job privacy_redrive_erase_busy "$PRIVACY_BUSY_PID"
expect privacy_redrive_erase_busy 55P03
got=$(sql <<<"SELECT c.status||'|'||r.status||'|'||(SELECT count(*) FROM app.data_rights_erasures WHERE tenant_id='$T' AND customer_id='$CUSTOMER')||'|'||(SELECT recipient FROM app.notification_deliveries WHERE tenant_id='$T' AND id='$NOTIFICATION') FROM app.customers c JOIN app.data_rights_exports r ON(r.tenant_id,r.customer_id)=(c.tenant_id,c.id) WHERE c.tenant_id='$T' AND c.id='$CUSTOMER' AND r.id='$REQUEST';")
echo "privacy redrive contention rollback: $got (expected active|pending|0|privacy-notify@example.invalid)"
[[ $got == 'active|pending|0|privacy-notify@example.invalid' ]] || fail=1
gate_open 83; wait
expect privacy_redrive_hold ok
tx privacy_redrive_erase_retry app_runtime "$T" "SELECT app.data_rights_erase_customer('$CUSTOMER','service','$ACTOR');"
expect privacy_redrive_erase_retry ok
got=$(sql <<<"SELECT d.status||'|'||d.last_error||'|'||(d.recipient='erased-'||d.id::text||'@invalid.test')||'|'||(SELECT count(*) FROM app.outbox_events e WHERE e.tenant_id='$T' AND e.payload=jsonb_build_object('notification_id',d.id) AND e.delivery_target='notifications.email' AND e.status='dead_letter' AND e.last_error='PRIVACY_ERASED') FROM app.notification_deliveries d WHERE d.tenant_id='$T' AND d.id='$NOTIFICATION';")
echo "privacy redrive contention retry: $got (expected dead_letter|PRIVACY_ERASED|true|2)"
[[ $got == 'dead_letter|PRIVACY_ERASED|true|2' ]] || fail=1
# The initial order scan cannot see an uncommitted order. An independent share
# holder keeps erasure at customer admission while insertion commits and a real
# financial writer locks that new order. The second scan MUST fail NOWAIT,
# allowing that writer's customer-dependent fanout and a later erase retry.
read -r T CUSTOMER REQUEST ACTOR <<<"$(privacy_fixture)"
PRIVACY_ORDER=$(sql <<<"SELECT uuidv7();")
gate_close 84;gate_close 85;gate_close 86
tx privacy_new_order_share postgres "$T" "SELECT id FROM app.customers WHERE tenant_id='$T' AND id='$CUSTOMER' FOR SHARE; $(park 84)" &
await privacy_new_order_share advisory
tx privacy_new_order_insert app_orders_executor "$T" "INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,customer_id,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor) VALUES('$T','$PRIVACY_ORDER',1,'c6d14da0-5835-56b1-9c58-b359221ed034',sha256('race'::bytea),'$CUSTOMER','USD',100,0,0,'{\"amount_minor\":0,\"currency\":\"USD\",\"address\":\"Synthetic private street\"}',100); $(park 85)" &
await privacy_new_order_insert advisory
tx privacy_new_order_erase_busy app_runtime "$T" "SELECT app.data_rights_erase_customer('$CUSTOMER','service','$ACTOR');" &
PRIVACY_BUSY_PID=$!
await privacy_new_order_erase_busy "${ROWLOCK[@]}"
gate_open 85;await privacy_new_order_insert gone
expect privacy_new_order_insert ok
tx privacy_new_order_transition app_orders_executor "$T" "SELECT id FROM app.orders WHERE tenant_id='$T' AND id='$PRIVACY_ORDER' FOR UPDATE; $(park 86) INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES('$T','order.paid',jsonb_build_object('order_id','$PRIVACY_ORDER'),'orders.events','privacy-new-order-race');" &
await privacy_new_order_transition advisory
gate_open 84;await privacy_new_order_share gone;await privacy_new_order_erase_busy gone;await_job privacy_new_order_erase_busy "$PRIVACY_BUSY_PID"
expect privacy_new_order_share ok;expect privacy_new_order_erase_busy 55P03
got=$(sql <<<"SELECT c.status||'|'||r.status||'|'||(SELECT count(*) FROM app.data_rights_erasures WHERE tenant_id='$T' AND customer_id='$CUSTOMER') FROM app.customers c JOIN app.data_rights_exports r ON(r.tenant_id,r.customer_id)=(c.tenant_id,c.id) WHERE c.tenant_id='$T' AND c.id='$CUSTOMER' AND r.id='$REQUEST';")
echo "privacy newly committed order rollback: $got (expected active|pending|0)"
[[ $got == 'active|pending|0' ]] || fail=1
gate_open 86;wait
expect privacy_new_order_transition ok
tx privacy_new_order_erase_retry app_runtime "$T" "SELECT app.data_rights_erase_customer('$CUSTOMER','service','$ACTOR');"
expect privacy_new_order_erase_retry ok
got=$(sql <<<"SELECT c.status||'|'||r.status||'|'||d.status||'|'||d.last_error||'|'||(d.recipient='erased-'||d.id::text||'@invalid.test') FROM app.customers c JOIN app.data_rights_exports r ON(r.tenant_id,r.customer_id)=(c.tenant_id,c.id) JOIN app.orders o ON(o.tenant_id,o.customer_id)=(c.tenant_id,c.id) JOIN app.notification_deliveries d ON(d.tenant_id,d.reference_id)=(o.tenant_id,o.id) WHERE c.tenant_id='$T' AND c.id='$CUSTOMER' AND r.id='$REQUEST';")
echo "privacy newly committed order retry: $got (expected disabled|revoked|dead_letter|PRIVACY_ERASED|true)"
[[ $got == 'disabled|revoked|dead_letter|PRIVACY_ERASED|true' ]] || fail=1

echo 'PASS: privacy single-use, export/erase and notification/redrive/new-order races preserve rollback and revocation'
# C-22a data rights races END

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


# C09b gates 80/81: controlled publication site-lock/CAS and ABA restore fence.
read -r CMS_T CMS_S <<<"$(sql <<'SQL' | tail -1
INSERT INTO app.tenants(name) VALUES('cms-race-80') RETURNING id AS t \gset
INSERT INTO app.organizations(tenant_id,display_name) VALUES(:'t','CMS race') RETURNING id AS o \gset
INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(:'t',:'o','CMS race') RETURNING id AS s \gset
SELECT :'t'||' '||:'s';
SQL
)"
CMS_A=cc09b000-0000-4000-8000-000000000080
CMS_ACTOR="SELECT set_config('app.actor_kind','user',true),set_config('app.actor_id','$CMS_A',true);"
CMS_D=$(sql <<SQL | tail -1
BEGIN; SELECT set_config('app.tenant_id','$CMS_T',true); SET LOCAL ROLE app_runtime; $CMS_ACTOR
SELECT app.cms_create_draft('$CMS_T','$CMS_S','user','$CMS_A','en','Race','Race')->>'draft_id' AS d \gset
SELECT app.cms_apply('$CMS_T','$CMS_S',:'d','user','$CMS_A',1,'save_page','{"page_key":"home","locale":"en","path":"/","document":{"title":"Race","description":"Race","blocks":[]},"media_ids":[]}');
COMMIT;
SELECT :'d';
SQL
)
CMS_PUBLISH="$CMS_ACTOR SELECT app.cms_publish('$CMS_T','$CMS_S','$CMS_D','user','$CMS_A',2,NULL,0,'race80');"
gate_close 80
tx cms_publish_winner app_runtime "$CMS_T" "$CMS_PUBLISH $(park 80)" &
await cms_publish_winner advisory
tx cms_publish_loser app_runtime "$CMS_T" "$CMS_PUBLISH" &
await cms_publish_loser "${ROWLOCK[@]}"
gate_open 80; wait
expect cms_publish_winner ok; expect cms_publish_loser CMS_PUBLICATION_STALE; expect cms_publish_loser 40001
got=$(sql <<<"SELECT s.publication_revision||'|'||d.status||'|'||(SELECT count(*) FROM app.site_releases WHERE tenant_id='$CMS_T' AND site_id='$CMS_S')||'|'||(SELECT count(*) FROM app.audit_events WHERE tenant_id='$CMS_T' AND operation='cms.publish') FROM app.sites s JOIN app.site_drafts d ON (s.tenant_id,s.id)=(d.tenant_id,d.site_id) WHERE s.tenant_id='$CMS_T' AND s.id='$CMS_S';")
echo "CMS gate80 publish winner: $got (expected 1|published|1|1)"
[[ $got == '1|published|1|1' ]] || fail=1
CMS_R=$(sql <<<"SELECT published_release_id FROM app.sites WHERE tenant_id='$CMS_T' AND id='$CMS_S';")
# Both contenders restore the identical target/pointer. UUID CAS alone would
# admit both; the publication epoch makes the second request stale (ABA proof).
CMS_RESTORE="$CMS_ACTOR SELECT app.cms_restore('$CMS_T','$CMS_S','user','$CMS_A','$CMS_R',1,'$CMS_R');"
gate_close 81
tx cms_restore_winner app_runtime "$CMS_T" "$CMS_RESTORE $(park 81)" &
await cms_restore_winner advisory
tx cms_restore_loser app_runtime "$CMS_T" "$CMS_RESTORE" &
await cms_restore_loser "${ROWLOCK[@]}"
gate_open 81; wait
expect cms_restore_winner ok; expect cms_restore_loser CMS_PUBLICATION_STALE; expect cms_restore_loser 40001
got=$(sql <<<"SELECT publication_revision||'|'||(published_release_id='$CMS_R')||'|'||(SELECT count(*) FROM app.site_releases WHERE tenant_id='$CMS_T' AND site_id='$CMS_S')||'|'||(SELECT count(*) FROM app.audit_events WHERE tenant_id='$CMS_T' AND operation='cms.restore') FROM app.sites WHERE tenant_id='$CMS_T' AND id='$CMS_S';")
echo "CMS gate81 restore ABA: $got (expected 2|true|1|1)"
[[ $got == '2|true|1|1' ]] || fail=1

# A publisher's validation read and a concurrent apply use site -> draft order.
# Park the editor after its site lock but before draft mutation; the publisher
# must wait at the site, then observe the stale draft revision without deadlock.
CMS_D=$(sql <<SQL | tail -1
BEGIN; SELECT set_config('app.tenant_id','$CMS_T',true); SET LOCAL ROLE app_runtime; $CMS_ACTOR
SELECT app.cms_create_draft('$CMS_T','$CMS_S','user','$CMS_A','en','Race','Race')->>'draft_id' AS d \gset
COMMIT; SELECT :'d';
SQL
)
gate_close 82
tx cms_apply_order app_runtime "$CMS_T" "$CMS_ACTOR SELECT id FROM app.sites WHERE tenant_id='$CMS_T' AND id='$CMS_S' FOR UPDATE; $(park 82) SELECT app.cms_apply('$CMS_T','$CMS_S','$CMS_D','user','$CMS_A',1,'set_metadata','{\"locale\":\"en\",\"title\":\"Edited\",\"description\":\"Race\"}');" &
await cms_apply_order advisory
tx cms_publish_order app_runtime "$CMS_T" "$CMS_ACTOR SELECT app.cms_read_draft('$CMS_T','$CMS_S','$CMS_D',1); SELECT app.cms_publish('$CMS_T','$CMS_S','$CMS_D','user','$CMS_A',1,'$CMS_R',2,'race82');" &
await cms_publish_order "${ROWLOCK[@]}"
gate_open 82; wait
expect cms_apply_order ok; expect cms_publish_order CMS_DRAFT_STALE; expect cms_publish_order 40001
got=$(sql <<<"SELECT s.publication_revision||'|'||d.revision||'|'||d.status||'|'||(SELECT count(*) FROM app.site_releases WHERE tenant_id='$CMS_T' AND site_id='$CMS_S') FROM app.sites s JOIN app.site_drafts d ON (s.tenant_id,s.id)=(d.tenant_id,d.site_id) WHERE s.tenant_id='$CMS_T' AND s.id='$CMS_S' AND d.id='$CMS_D';")
echo "CMS gate82 apply/publish lock order: $got (expected 2|2|open|1)"
[[ $got == '2|2|open|1' ]] || fail=1


# Checkout durable claim competes before provider IO. Both winner directions
# are synchronized by gates 90/91, with a witnessed contender row-lock wait.
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
SELECT (app.inventory_reserve('$SKU',1,'order','$OID','24ebea5b-19d7-5c87-91f1-339461604545',900)).id AS rid \gset
SELECT app.orders_create('$OID','c175822e-9019-56a3-9614-a2fe0568ec16',sha256('race'::bytea),NULL,'{"email":"race@example.invalid"}','USD',20,'{}',
 jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id','$SKU','sku','checkout-race','quantity',1,'unit_price_minor',100,
 'line_net_minor',100,'line_tax_minor',10,'line_total_minor',110,'reservation_id',:'rid','tax_snapshot','{"category":"standard"}'::jsonb)),'[]');
INSERT INTO app.checkout_sessions(tenant_id,id,actor_kind,actor_id,request_key,request_hash,cart_id,cart_version,order_id,currency,
 amount_minor,items_net_minor,tax_minor,shipping_minor,payable_version,address_snapshot)
 VALUES('$T','$CID','guest',repeat('a',64),'0a625bed-1d9b-5863-bc0e-db674e4d6106',sha256('start'::bytea),uuidv7(),1,'$OID','USD',130,100,10,20,1,'{}');
UPDATE app.checkout_sessions SET phase='ready',provider_ref='fake-race',start_result='{}' WHERE tenant_id='$T' AND id='$CID';
SQL
 if [[ $first_phase == confirming ]]; then second_phase=cancelling; first_kind=payment; second_kind=cancel; final=paid; stock=0; journals=1; outbox=3; race_gate=90
 else second_phase=confirming; first_kind=cancel; second_kind=payment; final=cancelled; stock=1; journals=0; outbox=2; race_gate=91; fi
 claim_first="WITH changed AS (UPDATE app.checkout_sessions SET phase='$first_phase',claim_kind='$first_kind',claim_key='23250dd6-9fb3-5ea8-bb29-56e793463b5e' WHERE tenant_id='$T' AND id='$CID' AND phase='ready' RETURNING id) SELECT 'C18_CLAIM_FIRST='||count(*) FROM changed;"
 claim_second="WITH changed AS (UPDATE app.checkout_sessions SET phase='$second_phase',claim_kind='$second_kind',claim_key='e270d8f8-1dcc-5011-9090-cb54dd78d9e1' WHERE tenant_id='$T' AND id='$CID' AND phase='ready' RETURNING id) SELECT 'C18_CLAIM_SECOND='||count(*) FROM changed;"
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


# Gate 92: two genuinely overlapping connections for each single-use auth command.
# Winner owns the customer lock and parks. Every contender must be observed
# waiting on the row/transaction lock, never merely spawned, before release.
for auth_case in verify reset rotate; do
 read -r T CUSTOMER PHC <<<"$(sql <<'SQL' | tail -1
INSERT INTO app.tenants(name) VALUES('customer-auth-gate92-race') RETURNING id AS t \gset
SELECT set_config('app.tenant_id',:'t',false);
SET ROLE app_customer_auth;
SELECT app.customer_prepare_registration('race@example.invalid',
 '$argon2id$v=19$m=19456,t=2,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU',
 sha256('verify'::bytea),clock_timestamp()+interval '1 hour');
RESET ROLE;
SELECT :'t'||' '||id||' '||
 '$argon2id$v=19$m=19456,t=2,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU'
 FROM app.customers WHERE tenant_id=:'t';
SQL
)"
 if [[ $auth_case != verify ]]; then
  tx "auth_${auth_case}_seed" app_customer_auth "$T" "SELECT app.customer_consume_verification(sha256('verify'::bytea));"
  expect "auth_${auth_case}_seed" ok
 fi
 if [[ $auth_case == reset ]]; then
  tx auth_reset_prepare app_customer_auth "$T" "SELECT app.customer_prepare_reset('race@example.invalid',sha256('reset'::bytea),clock_timestamp()+interval '1 hour');"
  expect auth_reset_prepare ok
 elif [[ $auth_case == rotate ]]; then
  tx auth_rotate_prepare app_customer_auth "$T" "SELECT app.customer_save_session('$CUSTOMER','$PHC',NULL,sha256('old-session'::bytea),clock_timestamp()+interval '1 day',clock_timestamp()+interval '1 hour');"
  expect auth_rotate_prepare ok
 fi
 gate_close 92
 auth_jobs=()
 for participant in $(seq 0 1); do
  auth_command="app.customer_consume_verification(sha256('verify'::bytea))"
  if [[ $auth_case == reset ]]; then
   changed_phc=${PHC/YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU/ZWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU}
   auth_command="app.customer_consume_reset(sha256('reset'::bytea),'$changed_phc')"
  elif [[ $auth_case == rotate ]]; then
   auth_command="(app.customer_save_session('$CUSTOMER','$PHC',sha256('old-session'::bytea),sha256('new-session-$participant'::bytea),clock_timestamp()+interval '1 day',clock_timestamp()+interval '1 hour') IS NOT NULL)"
  fi
  auth_park=''; [[ $participant == 0 ]] && auth_park=$(park 92)
  tx "auth_${auth_case}_$participant" app_customer_auth "$T" "SELECT 'AUTH_WIN='||($auth_command)::text; $auth_park" &
  auth_jobs+=("$!")
  [[ $participant != 0 ]] || await "auth_${auth_case}_0" advisory
 done
 auth_synchronized=0
 for attempt in $(seq 1 300); do
  blocked=$(sql <<<"SELECT count(*) FROM pg_stat_activity WHERE left(application_name,length('${RACE_PREFIX}auth_${auth_case}_'))='${RACE_PREFIX}auth_${auth_case}_' AND wait_event IN('transactionid','tuple');")
  if [[ $blocked == 1 ]]; then auth_synchronized=1; break; fi
  sleep 0.05
 done
 [[ $auth_synchronized == 1 ]] || { echo "FAIL: CUSTOMER_AUTH_92_SYNC $auth_case expected1 blocked, observed$blocked"; fail=1; }
 gate_open 92
 for participant in $(seq 0 1); do
  await_job "auth_${auth_case}_$participant" "${auth_jobs[$participant]}"
  expect "auth_${auth_case}_$participant" ok
  expected=false; [[ $participant != 0 ]] || expected=true
  result "auth_${auth_case}_$participant" "AUTH_WIN=$expected"
 done
 if [[ $auth_case == rotate ]]; then
  got=$(sql <<<"SELECT count(*) FROM app.customer_sessions WHERE tenant_id='$T' AND revoked_at IS NULL AND expires_at>clock_timestamp() AND idle_expires_at>clock_timestamp();")
 else
  auth_purpose=register; [[ $auth_case != reset ]] || auth_purpose=reset
  got=$(sql <<<"SELECT count(*) FROM app.customer_auth_tokens WHERE tenant_id='$T' AND purpose='$auth_purpose' AND consumed_at IS NOT NULL;")
 fi
 [[ $got == 1 ]] || { echo "FAIL: CUSTOMER_AUTH_92_END_STATE $auth_case expected1 observed$got"; fail=1; }
 echo "customer auth $auth_case: 2 synchronized connections (gate 92), exactly one winner"
done

# C-20a: shipment/cancellation/return all write the same Orders parent before
# checking child aggregates. Expected next versions deliberately bypass the
# stale-If-Match branch, so these witnesses exercise the quantity constraint.
fulfillment_fixture() {
 sql -v finalizer="${1:-false}" <<'SQL' | tail -1
INSERT INTO app.tenants(name,settlement_currency) VALUES('fulfillment-race','USD') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','race') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'p','race','default') RETURNING id AS sku \gset
SELECT set_config('app.tenant_id',:'t',false);
SET ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',5,'race receipt',uuidv7(),'inbound');
SET ROLE app_runtime;
SELECT uuidv7() AS oid,uuidv7() AS lid \gset
SELECT (app.inventory_reserve(:'sku',2,'order',:'oid','ab848f37-d8ab-5514-aa87-267367559ea8',900)).id AS rid \gset
SELECT app.orders_create(:'oid','8a6a9874-beec-532f-a9f6-75330fec23be',sha256('race'::bytea),NULL,'{"email":"race@example.invalid"}',
 'USD',2,'{"currency":"USD","amount_minor":2,"service":"fixture"}',
 jsonb_build_array(jsonb_build_object('id',:'lid','sku_id',:'sku','sku','race','quantity',2,
 'unit_price_minor',5,'line_net_minor',10,'line_tax_minor',2,'line_total_minor',12,'reservation_id',:'rid','tax_snapshot','{"category":"standard"}'::jsonb)),'[]');
\if :finalizer
RESET ROLE;
INSERT INTO app.ledger_accounts(tenant_id,code,currency,account_type) VALUES
 (:'t','revenue','USD','revenue'),(:'t','tax_payable','USD','liability'),(:'t','shipping_revenue','USD','revenue'),(:'t','cash','USD','asset');
INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment) VALUES(:'t','fake','c19d2-'||:'t','test');
SET ROLE app_runtime;
INSERT INTO app.checkout_sessions(tenant_id,id,actor_kind,actor_id,request_key,request_hash,cart_id,cart_version,order_id,currency,
 amount_minor,items_net_minor,tax_minor,shipping_minor,payable_version,address_snapshot)
 VALUES(:'t',uuidv7(),'guest',repeat('a',64),'25e1e155-6404-5fe1-afd1-eedc6079878c',sha256('c19d2'::bytea),uuidv7(),1,:'oid','USD',14,10,2,2,1,'{}');
UPDATE app.checkout_sessions SET phase='ready',provider_ref='captured-race',start_result='{}' WHERE tenant_id=:'t';
UPDATE app.checkout_sessions SET phase='confirming',claim_kind='payment',claim_key='25e1e155-6404-5fe1-afd1-eedc6079878c' WHERE tenant_id=:'t';
UPDATE app.checkout_sessions SET phase='paid',final_result='{}' WHERE tenant_id=:'t';
-- A paid Checkout alone is not original PSP authority. Seed the same mapped
-- capture admission/outcome used by Payments before the synchronous RMA posts.
SELECT * FROM app.resolve_fake_payment_account('c19d2-'||:'t');
SELECT app.lock_payment_stream('c19d2-'||:'t','captured-race');
SELECT app.admit_payment_inbox('c19d2-'||:'t','race-capture',sha256('race-capture'::bytea)) AS capture_inbox \gset
INSERT INTO app.payment_events(tenant_id,id,provider_account_id,provider_ref,event_key,claimed_order_id,order_version,status,amount_minor,currency)
 VALUES(:'t',:'capture_inbox','c19d2-'||:'t','captured-race',encode(sha256('race-capture'::bytea),'hex'),:'oid',1,'captured',14,'USD');
UPDATE app.payment_events SET applied=true WHERE tenant_id=:'t' AND id=:'capture_inbox';
SELECT app.finish_payment_inbox(:'capture_inbox',true);
\endif
SELECT app.orders_transition(:'oid','paid','race-paid',0);
RESET ROLE;
SELECT concat_ws(' ',:'t',:'oid',:'lid',:'sku');
SQL
}
read -r T OID LID SKU <<<"$(fulfillment_fixture)"
gate_close 120
tx fulfill_ship_a app_runtime "$T" "SELECT app.fulfillment_create_shipment('$OID',2,'6878c69f-58e8-50ca-a4e4-5ec874734786','manual','A','[{\"order_line_id\":\"$LID\",\"quantity\":2}]'); $(park 120)" &
await fulfill_ship_a advisory
tx fulfill_ship_b app_runtime "$T" "SELECT app.fulfillment_create_shipment('$OID',3,'92b972dd-20fa-508f-a25c-292515639821','manual','B','[{\"order_line_id\":\"$LID\",\"quantity\":1}]');" &
await fulfill_ship_b "${ROWLOCK[@]}"
gate_open 120; wait
expect fulfill_ship_a ok; expect fulfill_ship_b FULFILLMENT_OVERSHIP
[[ $(sql <<<"SELECT sum(quantity)||'|'||(SELECT on_hand FROM app.stock_levels WHERE tenant_id='$T' AND sku_id='$SKU') FROM app.shipment_lines WHERE tenant_id='$T';") == '2|3' ]] || fail=1

read -r T OID LID SKU <<<"$(fulfillment_fixture)"
gate_close 121
tx fulfill_cancel_a app_runtime "$T" "SELECT app.returns_create('$OID',2,'cbaf6164-0396-5ac4-bb27-ee1d48b49557','unshipped_cancel','race cancellation','[{\"order_line_id\":\"$LID\",\"quantity\":2}]'); $(park 121)" &
await fulfill_cancel_a advisory
tx fulfill_ship_cancel_b app_runtime "$T" "SELECT app.fulfillment_create_shipment('$OID',3,'7d574273-19bc-51ab-b045-d7b9ba5a8b18','manual','B','[{\"order_line_id\":\"$LID\",\"quantity\":1}]');" &
await fulfill_ship_cancel_b "${ROWLOCK[@]}"
gate_open 121; wait
expect fulfill_cancel_a ok; expect fulfill_ship_cancel_b FULFILLMENT_OVERSHIP
[[ $(sql <<<"SELECT sum(quantity)||'|'||(SELECT on_hand FROM app.stock_levels WHERE tenant_id='$T' AND sku_id='$SKU')||'|'||(SELECT count(*) FROM app.shipment_lines WHERE tenant_id='$T') FROM app.return_lines WHERE tenant_id='$T';") == '2|5|0' ]] || fail=1

read -r T OID LID SKU <<<"$(fulfillment_fixture)"
tx fulfill_return_seed app_runtime "$T" "SELECT app.fulfillment_create_shipment('$OID',2,'8c9b65f3-b3b3-5554-86f6-9e6507547384','manual','A','[{\"order_line_id\":\"$LID\",\"quantity\":2}]');"
expect fulfill_return_seed ok
SLID=$(sql <<<"SELECT id FROM app.shipment_lines WHERE tenant_id='$T' AND order_line_id='$LID';")
gate_close 122
tx fulfill_return_a app_runtime "$T" "SELECT app.returns_create('$OID',3,'a66e5b67-24ac-59b7-908c-b9f378aa847e','shipment_return','race return','[{\"order_line_id\":\"$LID\",\"shipment_line_id\":\"$SLID\",\"quantity\":2}]'); $(park 122)" &
await fulfill_return_a advisory
tx fulfill_return_b app_runtime "$T" "SELECT app.returns_create('$OID',4,'d4619d25-e941-5a79-a6d7-77a4d59b96f8','shipment_return','race return','[{\"order_line_id\":\"$LID\",\"shipment_line_id\":\"$SLID\",\"quantity\":1}]');" &
await fulfill_return_b "${ROWLOCK[@]}"
gate_open 122; wait
expect fulfill_return_a ok; expect fulfill_return_b FULFILLMENT_RETURN_ORDER_BOUND
[[ $(sql <<<"SELECT sum(quantity)||'|'||count(*) FROM app.return_lines WHERE tenant_id='$T';") == '2|1' ]] || fail=1

# RR reader fixes its old child snapshot before the winning shipment. Physical
# parent write forces serialization failure, not a stale sum that overships.
read -r T OID LID SKU <<<"$(fulfillment_fixture)"
gate_close 123
tx fulfill_rr_b app_runtime "$T" "SELECT count(*) FROM app.shipment_lines WHERE tenant_id='$T'; $(park 123) SELECT app.fulfillment_create_shipment('$OID',3,'76168db2-aff8-5b79-bb77-874876557e56','manual','B','[{\"order_line_id\":\"$LID\",\"quantity\":1}]');" "SET default_transaction_isolation='repeatable read';" &
await fulfill_rr_b advisory
tx fulfill_rr_a app_runtime "$T" "SELECT app.fulfillment_create_shipment('$OID',2,'83a22a96-99d0-5522-a250-8395dbec1a37','manual','A','[{\"order_line_id\":\"$LID\",\"quantity\":2}]');"
expect fulfill_rr_a ok
gate_open 123; wait
expect fulfill_rr_b 40001
[[ $(sql <<<"SELECT sum(quantity) FROM app.shipment_lines WHERE tenant_id='$T';") == 2 ]] || fail=1


# READ COMMITTED first-use identical keys must replay the winner, including its
# original If-Match and response, after blocking on the shared Orders parent.
read -r T OID LID SKU <<<"$(fulfillment_fixture)"
gate_close 124
same_ship="SELECT app.fulfillment_create_shipment('$OID',2,'8803e7f3-cb3a-524c-b367-a94841fcfcf2','manual','SAME','[{\"order_line_id\":\"$LID\",\"quantity\":1}]')->>'id';"
tx fulfill_same_ship_a app_runtime "$T" "$same_ship $(park 124)" &
await fulfill_same_ship_a advisory
tx fulfill_same_ship_b app_runtime "$T" "$same_ship" &
await fulfill_same_ship_b "${ROWLOCK[@]}"
gate_open 124; wait
expect fulfill_same_ship_a ok;expect fulfill_same_ship_b ok
ID=$(sql <<<"SELECT id FROM app.shipments WHERE tenant_id='$T';")
result fulfill_same_ship_a "$ID";result fulfill_same_ship_b "$ID"
[[ $(sql <<<"SELECT count(*)||'|'||sum(quantity) FROM app.shipment_lines WHERE tenant_id='$T';") == '1|1' ]] || fail=1

read -r T OID LID SKU <<<"$(fulfillment_fixture)"
gate_close 128
same_cancel="SELECT app.returns_create('$OID',2,'41cd0d63-85a1-5467-a3a1-fe0366619dc9','unshipped_cancel','same cancellation','[{\"order_line_id\":\"$LID\",\"quantity\":2}]')->>'id';"
tx fulfill_same_cancel_a app_runtime "$T" "$same_cancel $(park 128)" &
await fulfill_same_cancel_a advisory
tx fulfill_same_cancel_b app_runtime "$T" "$same_cancel" &
await fulfill_same_cancel_b "${ROWLOCK[@]}"
gate_open 128;wait
expect fulfill_same_cancel_a ok;expect fulfill_same_cancel_b ok
RID=$(sql <<<"SELECT id FROM app.returns WHERE tenant_id='$T';")
result fulfill_same_cancel_a "$RID";result fulfill_same_cancel_b "$RID"
[[ $(sql <<<"SELECT on_hand||'|'||(SELECT count(*) FROM app.inventory_movements WHERE tenant_id='$T' AND owner_type='return') FROM app.stock_levels WHERE tenant_id='$T';") == '5|1' ]] || fail=1
read -r INCOME TAX SHIPPING CASH <<<"$(sql <<SQL | tail -1
INSERT INTO app.ledger_accounts(tenant_id,code,currency,account_type) VALUES('$T','race-income','USD','revenue') RETURNING id AS income \gset
INSERT INTO app.ledger_accounts(tenant_id,code,currency,account_type) VALUES('$T','race-tax','USD','liability') RETURNING id AS tax \gset
INSERT INTO app.ledger_accounts(tenant_id,code,currency,account_type) VALUES('$T','race-shipping','USD','revenue') RETURNING id AS shipping \gset
INSERT INTO app.ledger_accounts(tenant_id,code,currency,account_type) VALUES('$T','race-cash','USD','asset') RETURNING id AS cash \gset
SELECT concat_ws(' ',:'income',:'tax',:'shipping',:'cash');
SQL
)"
gate_close 125
same_prepare="SELECT app.returns_prepare_refund('$RID',3,'e2e96e0b-e9ea-5cbe-bde1-a8db45c97e85','captured-race',1,'$INCOME','$TAX','$SHIPPING','$CASH')->>'id';"
tx fulfill_same_prepare_a app_returns_service "$T" "$same_prepare $(park 125)" &
await fulfill_same_prepare_a advisory
tx fulfill_same_prepare_b app_returns_service "$T" "$same_prepare" &
await fulfill_same_prepare_b "${ROWLOCK[@]}"
gate_open 125;wait
expect fulfill_same_prepare_a ok;expect fulfill_same_prepare_b ok
result fulfill_same_prepare_a "$RID";result fulfill_same_prepare_b "$RID"
[[ $(sql <<<"SELECT status||'|'||refund_minor||'|'||order_version FROM app.returns WHERE tenant_id='$T';") == 'refund_pending|14|4' ]] || fail=1
JID=$(sql <<SQL | tail -1
BEGIN;SELECT set_config('app.tenant_id','$T',true);SET LOCAL ROLE app_runtime;
SELECT app.post_ledger('$T','USD','return','$RID','refund',ARRAY['$INCOME','$TAX','$SHIPPING','$CASH']::uuid[],ARRAY['debit','debit','debit','credit'],ARRAY[10,2,2,14]::bigint[]) AS jid \gset
COMMIT;SELECT :'jid';
SQL
)
# Payments gate130: concurrent completion binds one provider refund to one RMA
# attempt. Existing parent/RMA locks fence both the budget and permanent identity.
gate_close 130
money_complete="SELECT app.payments_lock_return_checkout('$RID'); SELECT app.returns_lock_refund('$RID','e2e96e0b-e9ea-5cbe-bde1-a8db45c97e85'); SELECT app.payments_bind_return_refund('$RID','e2e96e0b-e9ea-5cbe-bde1-a8db45c97e85','refund-gate130','$JID'); SELECT app.returns_complete_refund('$RID','e2e96e0b-e9ea-5cbe-bde1-a8db45c97e85','captured-race','$JID')->>'id';"
tx payment_return_first app_returns_service "$T" "$money_complete $(park 130)" &
await payment_return_first advisory
tx payment_return_second app_returns_service "$T" "$money_complete" &
await payment_return_second "${ROWLOCK[@]}"
gate_open 130; wait
expect payment_return_first ok; expect payment_return_second ok
result payment_return_first "$RID"; result payment_return_second "$RID"
[[ $(sql <<<"SELECT count(*)||'|'||(SELECT refunded_minor FROM app.orders WHERE tenant_id='$T') FROM app.payment_return_refunds WHERE tenant_id='$T';") == '1|14' ]] || fail=1
echo 'PASS: Payments gate130 concurrent return refund binding and completion count once'
gate_close 126
same_complete="SELECT app.returns_complete_refund('$RID','e2e96e0b-e9ea-5cbe-bde1-a8db45c97e85','captured-race','$JID')->>'id';"
tx fulfill_same_complete_a app_returns_service "$T" "$same_complete $(park 126)" &
await fulfill_same_complete_a advisory
tx fulfill_same_complete_b app_returns_service "$T" "$same_complete" &
await fulfill_same_complete_b "${ROWLOCK[@]}"
gate_open 126;wait
expect fulfill_same_complete_a ok;expect fulfill_same_complete_b ok
result fulfill_same_complete_a "$RID";result fulfill_same_complete_b "$RID"
[[ $(sql <<<"SELECT state||'|'||version||'|'||refunded_minor FROM app.orders WHERE tenant_id='$T';") == 'refunded|5|14' ]] || fail=1

read -r T OID LID SKU <<<"$(fulfillment_fixture)"
tx fulfill_receive_seed app_runtime "$T" "SELECT app.fulfillment_create_shipment('$OID',2,'82b4e5a1-9218-5aff-ad46-e8bd4e86d818','manual','A','[{\"order_line_id\":\"$LID\",\"quantity\":2}]');"
expect fulfill_receive_seed ok
SLID=$(sql <<<"SELECT id FROM app.shipment_lines WHERE tenant_id='$T';")
tx fulfill_receive_request app_runtime "$T" "SELECT app.returns_create('$OID',3,'ea8b59d3-dae0-5f3b-902d-47450a20e0ee','shipment_return','receive race','[{\"order_line_id\":\"$LID\",\"shipment_line_id\":\"$SLID\",\"quantity\":2}]');"
expect fulfill_receive_request ok
RID=$(sql <<<"SELECT id FROM app.returns WHERE tenant_id='$T';")
gate_close 127
same_receive="SELECT app.returns_receive('$RID',4,'a02f55c7-e393-536d-9f42-155fa01a7a54',true)->>'id';"
tx fulfill_same_receive_a app_runtime "$T" "$same_receive $(park 127)" &
await fulfill_same_receive_a advisory
tx fulfill_same_receive_b app_runtime "$T" "$same_receive" &
await fulfill_same_receive_b "${ROWLOCK[@]}"
gate_open 127;wait
expect fulfill_same_receive_a ok;expect fulfill_same_receive_b ok
result fulfill_same_receive_a "$RID";result fulfill_same_receive_b "$RID"
[[ $(sql <<<"SELECT on_hand||'|'||(SELECT count(*) FROM app.inventory_movements WHERE tenant_id='$T' AND owner_type='return') FROM app.stock_levels WHERE tenant_id='$T';") == '5|1' ]] || fail=1
echo 'PASS: same-key first shipment/cancellation/receive/prepare/full-completion races (124-128)'

# Direct trusted INSERT isolates the trigger's physical parent-write fence:
# unlike normal commands these draft writes do not call Orders advance/version.
read -r T OID LID SKU <<<"$(fulfillment_fixture)"
gate_close 129
tx fulfill_direct_rr_b app_fulfillment_executor "$T" "SELECT count(*) FROM app.shipment_lines WHERE tenant_id='$T'; $(park 129)
 INSERT INTO app.shipments(tenant_id,order_id,creation_key,request_hash,carrier,tracking_number)
 VALUES('$T','$OID','6a1c63a0-684d-58a5-91b1-9aefc311d5ae',sha256('direct-b'::bytea),'manual','B') RETURNING id AS shid \gset
 INSERT INTO app.shipment_lines(tenant_id,shipment_id,order_id,order_line_id,quantity)
 VALUES('$T',:'shid','$OID','$LID',1);" "SET default_transaction_isolation='repeatable read';" &
await fulfill_direct_rr_b advisory
tx fulfill_direct_rr_a app_fulfillment_executor "$T" "INSERT INTO app.shipments(tenant_id,order_id,creation_key,request_hash,carrier,tracking_number)
 VALUES('$T','$OID','b1ff6bae-ef8f-53ce-923a-3ad7a6143939',sha256('direct-a'::bytea),'manual','A') RETURNING id AS shid \gset
 INSERT INTO app.shipment_lines(tenant_id,shipment_id,order_id,order_line_id,quantity)
 VALUES('$T',:'shid','$OID','$LID',2);"
expect fulfill_direct_rr_a ok
gate_open 129;wait
expect fulfill_direct_rr_b 40001
[[ $(sql <<<"SELECT sum(quantity)||'|'||(SELECT version FROM app.orders WHERE tenant_id='$T' AND id='$OID') FROM app.shipment_lines WHERE tenant_id='$T';") == '2|2' ]] || fail=1
echo 'PASS: direct trusted RR quantity trigger writes parent before child aggregates (129)'
echo 'PASS: fulfillment shipment/ship-cancel/return and RR stale-snapshot quantity races (120-123)'

# C19d2 gate131: a posted RMA is compensated once, across synchronized RC/RR/SERIALIZABLE contenders.
for isolation in 'read committed' 'repeatable read' 'serializable'; do
 read -r T OID LID SKU <<<"$(fulfillment_fixture true)"
 RID="$(sql <<SQL | tail -1
BEGIN;SELECT set_config('app.tenant_id','$T',true);SET LOCAL ROLE app_runtime;
SELECT app.returns_create('$OID',2,'925dbd9c-8b4d-5e17-9826-c342cd384326','unshipped_cancel','c19d2','[{"order_line_id":"$LID","quantity":2}]')->>'id' AS rid \gset
RESET ROLE;
SELECT id AS income FROM app.ledger_accounts WHERE tenant_id='$T' AND code='revenue' \gset
SELECT id AS tax FROM app.ledger_accounts WHERE tenant_id='$T' AND code='tax_payable' \gset
SELECT id AS shipping FROM app.ledger_accounts WHERE tenant_id='$T' AND code='shipping_revenue' \gset
SELECT id AS cash FROM app.ledger_accounts WHERE tenant_id='$T' AND code='cash' \gset
SET LOCAL ROLE app_returns_service;
SELECT app.returns_prepare_refund(:'rid',3,'23cb8d60-5e1d-5369-b898-3ce0b5b67cb1','captured-race',1,:'income',:'tax',:'shipping',:'cash');
SELECT app.payments_post_return_refund(:'rid','23cb8d60-5e1d-5369-b898-3ce0b5b67cb1','c19d2-refund') AS jid \gset
SELECT app.returns_complete_refund(:'rid','23cb8d60-5e1d-5369-b898-3ce0b5b67cb1','captured-race',:'jid');
COMMIT; SELECT :'rid';
SQL
 )"
 terminal="SELECT app.payments_finalize_refund(jsonb_build_object('tenant_id','$T','provider','fake','account_id','c19d2-$T','refund_id','c19d2-refund',
 'event_id','c19d2-terminal','provider_ref','captured-race','order_id','$OID','order_version',1,'amount_minor',14,'currency','USD','status','failed',
 'timestamp',extract(epoch FROM clock_timestamp())::bigint,'payload_hash',encode(sha256('terminal'::bytea),'hex')));"
 gate_close 131
 tx c19d2_first app_payments_ingress "$T" "$terminal $(park 131)" & first=$!
 await c19d2_first advisory
 tx c19d2_second app_payments_ingress "$T" "$terminal" "SET default_transaction_isolation='$isolation';" & second=$!
 await c19d2_second advisory
 gate_open 131
 await_job c19d2_first "$first";await_job c19d2_second "$second"
 expect c19d2_first ok
 if [[ $isolation == 'read committed' ]]; then expect c19d2_second ok; else expect c19d2_second 40001; fi
 tx c19d2_retry app_payments_ingress "$T" "$terminal";expect c19d2_retry ok
 got=$(sql <<<"SELECT o.state||'|'||o.refunded_minor||'|'||r.status||'|'||(SELECT count(*) FROM app.payment_refund_reversals WHERE tenant_id='$T')||'|'||c.refunded_net_minor||'|'||c.refunded_tax_minor||'|'||c.refunded_shipping_minor FROM app.orders o JOIN app.returns r ON(r.tenant_id,r.order_id)=(o.tenant_id,o.id) JOIN app.checkout_sessions c ON(c.tenant_id,c.order_id)=(o.tenant_id,o.id) WHERE o.tenant_id='$T';")
 if [[ $got == 'paid|0|refund_compensated|1|0|0|0' ]]; then
  echo "PASS C19D2 gate131 exactly-once compensation $isolation"
 else
  echo "FAIL C19D2_RACE $isolation $got";fail=1
 fi
done

# C19d3 gate132: recovery races signed finalization through the same financial owner.
for isolation in 'read committed' 'repeatable read' 'serializable'; do
 read -r T OID LID SKU <<<"$(fulfillment_fixture true)"
 RID="$(sql <<SQL | tail -1
BEGIN;SELECT set_config('app.tenant_id','$T',true);SET LOCAL ROLE app_runtime;
SELECT app.returns_create('$OID',2,'925dbd9c-8b4d-5e17-9826-c342cd384326','unshipped_cancel','c19d2','[{"order_line_id":"$LID","quantity":2}]')->>'id' AS rid \gset
RESET ROLE;
SELECT id AS income FROM app.ledger_accounts WHERE tenant_id='$T' AND code='revenue' \gset
SELECT id AS tax FROM app.ledger_accounts WHERE tenant_id='$T' AND code='tax_payable' \gset
SELECT id AS shipping FROM app.ledger_accounts WHERE tenant_id='$T' AND code='shipping_revenue' \gset
SELECT id AS cash FROM app.ledger_accounts WHERE tenant_id='$T' AND code='cash' \gset
SET LOCAL ROLE app_returns_service;
SELECT app.returns_prepare_refund(:'rid',3,'23cb8d60-5e1d-5369-b898-3ce0b5b67cb1','captured-race',1,:'income',:'tax',:'shipping',:'cash');
SELECT app.payments_post_return_refund(:'rid','23cb8d60-5e1d-5369-b898-3ce0b5b67cb1','c19d2-refund') AS jid \gset
SELECT app.returns_complete_refund(:'rid','23cb8d60-5e1d-5369-b898-3ce0b5b67cb1','captured-race',:'jid');
COMMIT; SELECT :'rid';
SQL
 )"
 terminal="SELECT app.payments_finalize_refund(jsonb_build_object('tenant_id','$T','provider','fake','account_id','c19d2-$T','refund_id','c19d2-refund',
 'event_id','c19d2-terminal','provider_ref','captured-race','order_id','$OID','order_version',1,'amount_minor',14,'currency','USD','status','failed',
 'timestamp',extract(epoch FROM clock_timestamp())::bigint,'payload_hash',encode(sha256('terminal'::bytea),'hex')));"
 fact=${terminal#"SELECT app.payments_finalize_refund("}; fact=${fact%");"}
 tx c19d3_retain app_payments_ingress "$T" "SELECT app.payments_retain_refund_case(($fact)||jsonb_build_object('reason','unmatched_refund'));";expect c19d3_retain ok
 CASE_ID=$(sql <<<"SELECT id FROM app.payment_refund_cases WHERE tenant_id='$T';" | tail -1)
 replay="SELECT app.payments_replay_refund_case('$CASE_ID',$fact);"
 gate_close 132
 tx c19d3_first app_payments_ingress "$T" "$replay $(park 132)" & first=$!
 await c19d3_first advisory
 tx c19d3_second app_payments_ingress "$T" "$terminal" "SET default_transaction_isolation='$isolation';" & second=$!
 await c19d3_second advisory
 gate_open 132
 await_job c19d3_first "$first";await_job c19d3_second "$second"
 expect c19d3_first ok
 if [[ $isolation == 'read committed' ]]; then expect c19d3_second ok; else expect c19d3_second 40001; fi
 tx c19d3_retry app_payments_ingress "$T" "$terminal";expect c19d3_retry ok
 got=$(sql <<<"SELECT o.state||'|'||o.refunded_minor||'|'||r.status||'|'||(SELECT count(*) FROM app.payment_refund_reversals WHERE tenant_id='$T')||'|'||c.refunded_net_minor||'|'||c.refunded_tax_minor||'|'||c.refunded_shipping_minor FROM app.orders o JOIN app.returns r ON(r.tenant_id,r.order_id)=(o.tenant_id,o.id) JOIN app.checkout_sessions c ON(c.tenant_id,c.order_id)=(o.tenant_id,o.id) WHERE o.tenant_id='$T';")
 if [[ $got == 'paid|0|refund_compensated|1|0|0|0' ]]; then
  echo "PASS C19D3 gate132 exactly-once compensation $isolation"
 else
  echo "FAIL C19D3_RECOVERY_RACE $isolation $got";fail=1
 fi
done

# B369 gate141: staff decision and terminal verification are independent facts,
# even when both have the same no-posting outcome. Exercise both stream-lock
# winner orders and retry complete commands after serialization failures.
for isolation in 'read committed' 'repeatable read' 'serializable'; do
 for first_fact in manual terminal; do
  read -r T OID LID SKU <<<"$(fulfillment_fixture true)"
  read -r ACTOR CREDENTIAL <<<"$(sql <<SQL | tail -1
INSERT INTO app.users(email,password_hash) VALUES('b369-'||uuidv7()::text||'@example.invalid','fixture') RETURNING id AS actor \gset
INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES('$T',:'actor','owner');
INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES(:'actor',sha256(:'actor'::bytea),clock_timestamp()+interval '1 hour') RETURNING id AS credential \gset
SELECT :'actor'||' '||:'credential';
SQL
)"
  fact="jsonb_build_object('tenant_id','$T','provider','fake','account_id','c19d2-$T','refund_id','b369-refund','event_id','b369-terminal','provider_ref','captured-race','order_id','$OID','order_version',1,'amount_minor',14,'currency','USD','status','failed','payload_hash',encode(sha256('b369-terminal'::bytea),'hex'))"
  tx b369_retain app_payments_ingress "$T" "SELECT app.payments_retain_refund_case(($fact)||jsonb_build_object('reason','unmatched_refund'));"
  expect b369_retain ok
  CASE_ID=$(sql <<<"SELECT id FROM app.payment_refund_cases WHERE tenant_id='$T';")
  manual="SELECT app.payments_resolve_refund_case('$CASE_ID',jsonb_build_object('user_id','$ACTOR','kind','session','credential_id','$CREDENTIAL','reason','no_posting_required','evidence_hash',encode(sha256('staff'::bytea),'hex')));"
  terminal="SELECT app.payments_replay_refund_case('$CASE_ID',$fact);"
  if [[ $first_fact == manual ]]; then first_sql=$manual; second_sql=$terminal; expected_order='no_posting_required,verified_terminal'
  else first_sql=$terminal; second_sql=$manual; expected_order='verified_terminal,no_posting_required'; fi
  gate_close 141
  tx b369_first app_payments_ingress "$T" "$first_sql $(park 141)" & first_pid=$!
  await b369_first advisory
  tx b369_second app_payments_ingress "$T" "$second_sql" "SET default_transaction_isolation='$isolation';" & second_pid=$!
  await b369_second advisory
  gate_open 141
  await_job b369_first "$first_pid"; await_job b369_second "$second_pid"
  expect b369_first ok
  if [[ -s $ERRDIR/b369_second ]]; then expect b369_second 40001; fi
  tx b369_retry app_payments_ingress "$T" "$second_sql" "SET default_transaction_isolation='$isolation';"
  expect b369_retry ok
  got=$(sql <<<"SELECT count(*) FILTER(WHERE reason='no_posting_required')||'|'||count(*) FILTER(WHERE reason='verified_terminal')||'|'||bool_and(outcome='resolved_no_posting')||'|'||string_agg(reason,',' ORDER BY created_at) FROM app.payment_refund_case_resolutions WHERE tenant_id='$T' AND case_id='$CASE_ID';")
  [[ $got == "1|1|true|$expected_order" ]] || { echo "FAIL B369_CONCURRENT_AUDIT_RED $isolation/$first_fact: $got"; fail=1; }
  tx b369_replay app_payments_ingress "$T" "$manual $terminal"; expect b369_replay ok
  [[ $(sql <<<"SELECT count(*) FROM app.payment_refund_case_resolutions WHERE tenant_id='$T' AND case_id='$CASE_ID';") == 2 ]] || { echo 'FAIL B369_CONCURRENT_REPLAY_RED'; fail=1; }
  echo "B369 gate141 $isolation/$first_fact witness: $got"
 done
done

# C-19a gates93/94: actual inbox unique-key contention and public stream lock.
# Separate connections must be observed blocked before release; no timed ordering.
T=$(sql <<<"INSERT INTO app.tenants(name) VALUES('payments-gate93') RETURNING id;" | tail -1)
ACCOUNT="c19-race-$T"
sql <<<"INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment,enabled) VALUES('$T','fake','$ACCOUNT','test',true);" >/dev/null
inbox_insert="INSERT INTO app.webhook_inbox(tenant_id,provider,provider_account_id,environment,provider_event_id,payload_hash,payload_reference) VALUES('$T','fake','$ACCOUNT','test','race-event',sha256('same'::bytea),'payment:race') ON CONFLICT (tenant_id,provider,provider_account_id,environment,provider_event_id) DO NOTHING RETURNING 'C19_INBOX_INSERTED';"
gate_close 93
tx payment_inbox_first app_runtime "$T" "$inbox_insert $(park 93)" &
await payment_inbox_first advisory
tx payment_inbox_second app_runtime "$T" "$inbox_insert" &
await payment_inbox_second "${ROWLOCK[@]}"
gate_open 93; wait
expect payment_inbox_first ok; expect payment_inbox_second ok
result payment_inbox_first C19_INBOX_INSERTED
if [[ $(cat "$ERRDIR/payment_inbox_second.out") == *C19_INBOX_INSERTED* ]]; then
 echo 'FAIL: C19 duplicate inbox inserted twice'; fail=1
fi
got=$(sql <<<"SELECT count(*)||'|'||bool_and(payload_hash=sha256('same'::bytea)) FROM app.webhook_inbox WHERE tenant_id='$T' AND provider_account_id='$ACCOUNT' AND provider_event_id='race-event';")
echo "payments gate93 duplicate inbox: $got (expected 1|true)"
[[ $got == '1|true' ]] || fail=1
# The production lock function is the single authority for stream lock identity.
# A different provider intent progresses while an identical intent is blocked.
gate_close 94
tx payment_stream_first app_runtime "$T" "SELECT app.lock_payment_stream('$ACCOUNT','intent-one'); $(park 94)" &
await payment_stream_first advisory
tx payment_stream_second app_runtime "$T" "SELECT app.lock_payment_stream('$ACCOUNT','intent-one'); SELECT 'C19_STREAM_SECOND';" &
await payment_stream_second advisory
tx payment_stream_other app_runtime "$T" "SELECT app.lock_payment_stream('$ACCOUNT','intent-two'); SELECT 'C19_STREAM_OTHER';" &
payment_other_pid=$!
await_job payment_stream_other "$payment_other_pid"
expect payment_stream_other ok; result payment_stream_other C19_STREAM_OTHER
gate_open 94; wait
expect payment_stream_first ok; expect payment_stream_second ok
result payment_stream_second C19_STREAM_SECOND

# FIX-44 artifact sweep/build serialize before customer/request locks, including
# a pending (rolled-back build) capability. No sleep-based ordering.
read -r T CUSTOMER REQUEST ACTOR <<<"$(privacy_fixture)"
gate_close 95
tx privacy_artifact_build app_runtime "$T" "SELECT app.data_rights_worker_info('$REQUEST'); $(park 95) SELECT app.data_rights_complete_export('$REQUEST',sha256('token'::bytea),sha256('content'::bytea),date_trunc('second',clock_timestamp())+interval '10 minutes');" &
await privacy_artifact_build advisory
tx privacy_artifact_sweep app_runtime "$T" "SELECT 'PRIVACY_ARTIFACT_LIVE='||app.data_rights_artifact_live('$REQUEST');" &
await privacy_artifact_sweep advisory
gate_open 95; wait
expect privacy_artifact_build ok; expect privacy_artifact_sweep ok
result privacy_artifact_sweep PRIVACY_ARTIFACT_LIVE=true
gate_close 96
tx privacy_artifact_sweep_hold app_runtime "$T" "SELECT app.data_rights_artifact_live('$REQUEST'); $(park 96)" &
await privacy_artifact_sweep_hold advisory
tx privacy_artifact_build_wait app_runtime "$T" "SELECT app.data_rights_worker_info('$REQUEST'); SELECT 'PRIVACY_BUILD_AFTER_SWEEP';" &
await privacy_artifact_build_wait advisory
gate_open 96; wait
expect privacy_artifact_sweep_hold ok; expect privacy_artifact_build_wait ok
result privacy_artifact_build_wait PRIVACY_BUILD_AFTER_SWEEP
# C-18b quote conversion gates 100-103. Each contender mirrors the public
# caller-owned transaction: lock, receipt lookup, reserve/create only if absent,
# then bind. Fixture helpers are session-local SQL, never production commands.
quote_conversion_fixture() {
 sql <<'SQL' | tail -1
BEGIN;
INSERT INTO app.tenants(name) VALUES('quote-conversion-race') RETURNING id AS t \gset
INSERT INTO app.organizations(tenant_id,display_name) VALUES(:'t','R') RETURNING id AS org \gset
INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(:'t',:'org','R') RETURNING id AS site \gset
INSERT INTO app.users(email,password_hash) VALUES(uuidv7()::text||'@race.invalid','fixture') RETURNING id AS staff \gset
INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(:'t',:'staff','admin');
INSERT INTO app.customers(tenant_id,email) VALUES(:'t',uuidv7()::text||'@race.invalid') RETURNING id AS buyer \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'t',uuidv7()::text||'@race.invalid') RETURNING id AS approver \gset
INSERT INTO app.buyer_companies(tenant_id,name) VALUES(:'t','R') RETURNING id AS company \gset
INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code)
 VALUES(:'t',:'company',:'buyer','purchaser'),(:'t',:'company',:'approver','approver');
INSERT INTO app.inquiries(tenant_id,site_id,buyer_email,message,buyer_company_id)
 VALUES(:'t',:'site','race@example.invalid','race',:'company') RETURNING id AS inquiry \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','race') RETURNING id AS product \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'product','race-sku','default') RETURNING id AS sku \gset
SELECT set_config('app.tenant_id',:'t',true),set_config('qc.t',:'t',false),set_config('qc.site',:'site',true),set_config('qc.inquiry',:'inquiry',true),
 set_config('qc.company',:'company',false),set_config('qc.staff',:'staff',true),set_config('qc.buyer',:'buyer',false),set_config('qc.approver',:'approver',true),
 set_config('qc.product',:'product',true),set_config('qc.sku',:'sku',false);
CREATE FUNCTION pg_temp.qc_quote() RETURNS uuid LANGUAGE plpgsql AS $$
DECLARE t uuid:=current_setting('qc.t')::uuid; q uuid; BEGIN
 q:=app.create_quote(t,current_setting('qc.site')::uuid,current_setting('qc.inquiry')::uuid,current_setting('qc.company')::uuid,current_setting('qc.staff')::uuid,
 'USD',100,10,5,115,'{"shipping":{"ZoneID":"zone","RateID":"rate","Method":"fixed","Charge":{"Amount":5,"Currency":"USD"}}}',clock_timestamp()+interval '1 hour',
 jsonb_build_array(jsonb_build_object('line_no',1,'product_id',current_setting('qc.product')::uuid,'variant_id',current_setting('qc.sku')::uuid,
 'description','race','quantity',2,'unit_price',50,'line_total',100,'snapshot',jsonb_build_object('id','1','amounts',jsonb_build_object('net',100,'tax',10,'gross',110),
 'snapshot',jsonb_build_object('currency','USD','category','standard','reason','fixture')))));
 PERFORM app.transition_quote(t,q,1,'send','staff',current_setting('qc.staff')::uuid);
 PERFORM app.transition_quote(t,q,2,'accept','customer',current_setting('qc.buyer')::uuid);
 PERFORM app.transition_quote(t,q,3,'approve','customer',current_setting('qc.approver')::uuid); RETURN q;
END $$;
SET LOCAL ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',20,'quote race',uuidv7(),'inbound');
SET LOCAL ROLE app_runtime;
SELECT set_config('qc.q1',pg_temp.qc_quote()::text,false),set_config('qc.q2',pg_temp.qc_quote()::text,false);
COMMIT;
SELECT concat_ws(' ',current_setting('qc.t'),current_setting('qc.company'),current_setting('qc.buyer'),current_setting('qc.sku'),current_setting('qc.q1'),current_setting('qc.q2'));
SQL
}
quote_conversion_call() { # quote UUID, caller key; all identifiers come from fixture SQL
 cat <<SQL
DO \$convert\$ DECLARE c app.quote_conversions; o app.orders; r app.inventory_reservations; oid uuid; BEGIN
 PERFORM app.quote_conversion_lock('$QT','$1','$QC','$QB');
 SELECT * INTO c FROM app.quote_conversions WHERE tenant_id='$QT' AND quote_id='$1';
 IF FOUND THEN oid:=c.order_id;
 ELSE
  oid:=uuidv7(); r:=app.inventory_reserve('$QS',2,'order',oid,(md5(('quote-race-reserve:'||oid)::text)::uuid::text),900);
  o:=app.orders_create(oid,(md5(('quote-race-create:'||oid)::text)::uuid::text),sha256(oid::text::bytea),'$QB',NULL,'USD',5,
  '{"ZoneID":"zone","RateID":"rate","Method":"fixed","Charge":{"Amount":5,"Currency":"USD"}}',
  jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id','$QS'::uuid,'sku','race-sku','quantity',2,'unit_price_minor',50,
  'line_net_minor',100,'line_tax_minor',10,'line_total_minor',110,'reservation_id',r.id,'tax_snapshot',jsonb_build_object('currency','USD','category','standard','reason','fixture'))),'[]');
 END IF;
 c:=app.quote_conversion_bind('$QT','$1','$QC','$QB',oid,4,'$2',sha256('$1'::bytea));
 PERFORM set_config('qc.result',c.order_id::text,true);
END \$convert\$;
SELECT 'converted='||current_setting('qc.result');
SQL
}
quote_conversion_count() {
 local got
 got=$(sql <<<"SELECT (SELECT count(*) FROM app.orders WHERE tenant_id='$QT')||'|'||(SELECT count(*) FROM app.quote_conversions WHERE tenant_id='$QT')||'|'||(SELECT count(*) FROM app.outbox_events WHERE tenant_id='$QT' AND event_type='quote.converted')||'|'||(SELECT count(*) FROM app.inventory_reservations WHERE tenant_id='$QT')||'|'||(SELECT reserved FROM app.stock_levels WHERE tenant_id='$QT' AND sku_id='$QS');")
 echo "quote conversion $1: $got (expected $2)"
 [[ $got == "$2" ]] || fail=1
}
for mode in duplicate different_key two_quotes_one_key; do
 read -r QT QC QB QS QQ QQ2 <<<"$(quote_conversion_fixture)"
 gate=100; second_quote=$QQ; second_key=3b86f942-cb7c-5c60-830f-6aa888010b1e; marker=ok
 if [[ $mode == different_key ]]; then gate=101; second_key=e138b438-607f-5312-bfd5-71e5b050e4be; marker=QUOTE_CONVERSION_ALREADY_CONVERTED; fi
 if [[ $mode == two_quotes_one_key ]]; then gate=103; second_quote=$QQ2; marker=QUOTE_CONVERSION_KEY_REUSED; fi
 first=$(quote_conversion_call "$QQ" 3b86f942-cb7c-5c60-830f-6aa888010b1e); second=$(quote_conversion_call "$second_quote" "$second_key")
 gate_close "$gate"
 tx conversion_first app_runtime "$QT" "$first $(park "$gate")" &
 await conversion_first advisory
 tx conversion_second app_runtime "$QT" "$second" &
 await conversion_second "${ROWLOCK[@]}"
 gate_open "$gate"; wait
 expect conversion_first ok; expect conversion_second "$marker"
 if [[ $mode == duplicate ]]; then
  receipt=$(sed -n 's/^converted=//p' "$ERRDIR/conversion_first.out")
  [[ -n $receipt ]] || { echo 'FAIL: quote conversion missing receipt'; fail=1; }
  result conversion_second "converted=$receipt"
 fi
 quote_conversion_count "$mode" '1|1|1|1|2'
done
# Gate 102: an approved quote expires while a contender waits for its row lock.
read -r QT QC QB QS QQ QQ2 <<<"$(quote_conversion_fixture)"
sql <<<"ALTER TABLE app.quotes DISABLE TRIGGER quote_guard; UPDATE app.quotes SET expires_at=clock_timestamp()+interval '8 seconds' WHERE tenant_id='$QT' AND id='$QQ'; ALTER TABLE app.quotes ENABLE TRIGGER quote_guard;" >/dev/null
gate_close 102
tx conversion_expiry_holder app_runtime "$QT" "SELECT app.quote_conversion_lock('$QT','$QQ','$QC','$QB'); $(park 102)" &
await conversion_expiry_holder advisory
tx conversion_expiry_waiter app_runtime "$QT" "$(quote_conversion_call "$QQ" 3b86f942-cb7c-5c60-830f-6aa888010b1e)" &
await conversion_expiry_waiter "${ROWLOCK[@]}"
expiry_seen=false
for _ in $(seq 1 240); do
 [[ $(sql <<<"SELECT expires_at<=clock_timestamp() FROM app.quotes WHERE tenant_id='$QT' AND id='$QQ';") == t ]] && { expiry_seen=true; break; }
 sleep 0.05
done
[[ $expiry_seen == true ]] || { echo 'FAIL: quote conversion expiry witness timed out'; fail=1; }
gate_open 102; wait
expect conversion_expiry_holder ok; expect conversion_expiry_waiter QUOTE_CONVERSION_NOT_APPROVED_OR_EXPIRED
quote_conversion_count expiry_after_lock_wait '0|0|0|0|0'
# End C-18b quote conversion gates.

# ADMIN-PRICING-1 gate110: list CAS must be rechecked after an actual row-lock wait.
read -r PT PL PS <<<"$(sql <<'SQL'
INSERT INTO app.tenants(name,settlement_currency) VALUES('staff-price-race','USD') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','STAFF-RACE') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'p','STAFF-RACE','') RETURNING id AS sku \gset
INSERT INTO app.price_lists(tenant_id,currency,starts_at) VALUES(:'t','USD',clock_timestamp()) RETURNING id AS l \gset
SELECT :'t'||' '||:'l'||' '||:'sku';
SQL
)"
gate_close 110
tx staff_price_first app_runtime "$PT" "SELECT app.pricing_write_entry('$PL','$PS',1,111,1); $(park 110)" &
await staff_price_first advisory
tx staff_price_stale app_runtime "$PT" "SELECT app.pricing_write_entry('$PL','$PS',1,222,1);" &
await staff_price_stale "${ROWLOCK[@]}"
gate_open 110; wait
expect staff_price_first ok; expect staff_price_stale PRICING_VERSION_CONFLICT
[[ $(sql <<<"SELECT p.version||'|'||e.unit_amount FROM app.price_lists p JOIN app.price_entries e ON e.tenant_id=p.tenant_id AND e.price_list_id=p.id WHERE p.tenant_id='$PT' AND p.id='$PL';") == '2|111' ]] || fail=1

# b-pricing gate142: candidate invalidation while a first CMS publish is uncommitted.
# The Go statement-boundary gate also checks rebuilt serving bytes; this gate proves
# the SQL trigger commits with no publication/serving lock on the price writer.
read -r BP_SITE BP_PRODUCT <<<"$(sql <<SQL
INSERT INTO app.organizations(tenant_id,display_name) VALUES('$PT','candidate') RETURNING id AS o \gset
INSERT INTO app.sites(tenant_id,organization_id,name,default_locale) VALUES('$PT',:'o','candidate','en') RETURNING id AS s \gset
SELECT product_id AS p FROM app.product_variants WHERE tenant_id='$PT' AND id='$PS' \gset
UPDATE app.products SET lifecycle_status='ready' WHERE tenant_id='$PT' AND id=:'p';
UPDATE app.product_variants SET lifecycle_status='ready' WHERE tenant_id='$PT' AND id='$PS';
UPDATE app.price_lists SET status='active' WHERE tenant_id='$PT' AND id='$PL';
INSERT INTO app.product_content_revisions(tenant_id,product_id,locale,revision,title,seo) VALUES('$PT',:'p','en',1,'Candidate','{}');
INSERT INTO app.product_listings(tenant_id,site_id,product_id,locale,slug,status,published_content_revision)
 VALUES('$PT',:'s',:'p','en','candidate','published',1);
DELETE FROM app.serving_invalidations WHERE tenant_id='$PT';
SELECT :'s'||' '||:'p';
SQL
)"
BP_DRAFT=$(sql <<SQL | tail -1
BEGIN; SELECT set_config('app.tenant_id','$PT',true); SET LOCAL ROLE app_runtime; $CMS_ACTOR
SELECT app.cms_create_draft('$PT','$BP_SITE','user','$CMS_A','en','Candidate','')->>'draft_id' AS d \gset
SELECT app.cms_apply('$PT','$BP_SITE',:'d','user','$CMS_A',1,'save_page','{"page_key":"home","locale":"en","path":"/","document":{"title":"Candidate","description":"","blocks":[]},"media_ids":[]}');
COMMIT;
SELECT :'d';
SQL
)
gate_close 142
tx bpricing_publish app_runtime "$PT" "$CMS_ACTOR
DO \$bp\$ DECLARE r jsonb; BEGIN
 r:=app.cms_publish('$PT','$BP_SITE','$BP_DRAFT','user','$CMS_A',2,NULL,0,'candidate142');
 PERFORM app.cms_seal_skus('$PT','$BP_SITE',(r->>'release_id')::uuid,
  jsonb_build_array(jsonb_build_object('product_id','$BP_PRODUCT','sku_id','$PS','sku_code','STAFF-RACE','option_values','[]'::jsonb)));
END \$bp\$; $(park 142)" &
await bpricing_publish advisory
tx bpricing_change postgres "$PT" "UPDATE app.price_entries SET unit_amount=333 WHERE tenant_id='$PT' AND price_list_id='$PL' AND sku_id='$PS';" &
bpricing_change_pid=$!
await_job bpricing_change "$bpricing_change_pid"
expect bpricing_change ok
[[ $(sql <<<"SELECT count(*) FROM app.serving_invalidations WHERE tenant_id='$PT' AND site_id='$BP_SITE' AND reason='price_entry';") == 1 ]] || {
 echo 'FAIL: PUBLISH_CANDIDATE_RACE142 lost invalidation before membership commit'; fail=1;
}
gate_open 142; wait
expect bpricing_publish ok
echo 'PASS: PUBLISH_CANDIDATE_RACE142 price mutation did not wait for publication'

# MDEL-01 gates 150-154: the shared prefix orders ordinary writers,
# transition contenders, admitted drains and cancellation at the DB deadline.
mdel_fixture() {
 sql <<'SQL' | tail -1
INSERT INTO app.tenants(name) VALUES('mdel01-race') RETURNING id AS t \gset
INSERT INTO app.users(email,password_hash) VALUES('mdelrace-'||uuidv7()||'@invalid.test','fixture') RETURNING id AS u \gset
INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(:'t',:'u','owner');
INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES(:'u',sha256(uuidv7()::text::bytea),clock_timestamp()+interval '1 day') RETURNING id AS sess \gset
SELECT :'t'||' '||:'u'||' '||:'sess'||' '||uuidv7()||' '||uuidv7();
SQL
}
MDEL_ZERO='{"paid_unfulfilled":0,"fulfillment_return":0,"payment_refund":0,"checkout_capture":0,"refund_reconciliation":0,"pending_payout":0,"unsettled_provider_balance":0,"legal_hold":0}'
mdel_call() {
 echo "SELECT app.tenant_lifecycle_transition('$MT','$MR','$MU','$MS','$1','$MK',sha256('$1'::bytea),$2,'$MDEL_ZERO');"
}
read -r MT MU MS MR MK <<<"$(mdel_fixture)"
gate_close 150
tx mdel_writer app_runtime "$MT" "SELECT app.tenant_lifecycle_write_fence('$MT'); INSERT INTO app.organizations(tenant_id,display_name) VALUES('$MT','admitted'); $(park 150)" &
await mdel_writer advisory
tx mdel_request postgres "$MT" "$(mdel_call request 0)" &
await mdel_request "${ROWLOCK[@]}"
gate_open 150; wait
expect mdel_writer ok; expect mdel_request ok
[[ $(sql <<<"SELECT state||'|'||(SELECT count(*) FROM app.organizations WHERE tenant_id='$MT') FROM app.tenant_lifecycles WHERE tenant_id='$MT';") == 'cooling_off|1' ]] || { echo 'FAIL: MDEL01_REQUEST_WRITER_RACE'; fail=1; }

read -r MT MU MS MR MK <<<"$(mdel_fixture)"
gate_close 151
tx mdel_transition postgres "$MT" "$(mdel_call request 0) $(park 151)" &
await mdel_transition advisory
tx mdel_late_writer app_runtime "$MT" "SELECT app.tenant_lifecycle_write_fence('$MT'); INSERT INTO app.organizations(tenant_id,display_name) VALUES('$MT','forbidden');" &
await mdel_late_writer "${ROWLOCK[@]}"
gate_open 151; wait
expect mdel_transition ok; expect mdel_late_writer 'tenant deletion write fence'
[[ $(sql <<<"SELECT count(*) FROM app.organizations WHERE tenant_id='$MT';") == 0 ]] || { echo 'FAIL: MDEL01_LATE_WRITE'; fail=1; }

# Move the persisted deadline using the DB clock; ordering never uses a sleep.
sql <<<"UPDATE app.tenant_lifecycles SET requested_at=clock_timestamp()-interval '721 hours',execute_after=clock_timestamp()-interval '1 hour' WHERE tenant_id='$MT';" >/dev/null
gate_close 152
tx mdel_drain postgres "$MT" "SELECT app.tenant_lifecycle_drain_fence('$MT',1); $(park 152)" &
await mdel_drain advisory
tx mdel_execute postgres "$MT" "$(mdel_call execute 1)" &
await mdel_execute "${ROWLOCK[@]}"
gate_open 152; wait
expect mdel_drain ok; expect mdel_execute ok
tx mdel_stale_drain postgres "$MT" "SELECT app.tenant_lifecycle_drain_fence('$MT',1);"
expect mdel_stale_drain 'tenant deletion drain fence'
[[ $(sql <<<"SELECT state||'|'||epoch FROM app.tenant_lifecycles WHERE tenant_id='$MT';") == 'executing|2' ]] || { echo 'FAIL: MDEL01_EXECUTE_DRAIN_RACE'; fail=1; }

# A request committed before cancellation holds the exclusive lifecycle row;
# after it releases, cancel must serialize against the persisted epoch/deadline.
read -r MT MU MS MR MK <<<"$(mdel_fixture)"
gate_close 153
tx mdel_before_cancel postgres "$MT" "$(mdel_call request 0) $(park 153)" &
await mdel_before_cancel advisory
MK=$(sql <<<"SELECT uuidv7();")
tx mdel_cancel postgres "$MT" "$(mdel_call cancel 1)" &
await mdel_cancel "${ROWLOCK[@]}"
gate_open 153; wait
expect mdel_before_cancel ok; expect mdel_cancel ok
[[ $(sql <<<"SELECT state||'|'||epoch FROM app.tenant_lifecycles WHERE tenant_id='$MT';") == 'active|2' ]] || { echo 'FAIL: MDEL01_CANCEL_SERIALIZATION'; fail=1; }

read -r MT MU MS MR MK <<<"$(mdel_fixture)"
tx mdel_expiry_request postgres "$MT" "$(mdel_call request 0)"
expect mdel_expiry_request ok
sql <<<"UPDATE app.tenant_lifecycles SET requested_at=clock_timestamp()-interval '721 hours',execute_after=clock_timestamp()-interval '1 hour' WHERE tenant_id='$MT';" >/dev/null
gate_close 154
tx mdel_expiry_execution postgres "$MT" "$(mdel_call execute 1) $(park 154)" &
await mdel_expiry_execution advisory
MK=$(sql <<<"SELECT uuidv7();")
tx mdel_expiry_cancel postgres "$MT" "$(mdel_call cancel 1)" &
await mdel_expiry_cancel "${ROWLOCK[@]}"
gate_open 154; wait
expect mdel_expiry_execution ok; expect mdel_expiry_cancel 'stale lifecycle operation'
[[ $(sql <<<"SELECT state||'|'||epoch FROM app.tenant_lifecycles WHERE tenant_id='$MT';") == 'executing|2' ]] || { echo 'FAIL: MDEL01_EXPIRY_CANCEL_RACE'; fail=1; }
echo 'PASS: MDEL01 gates 150-154 writer/request, late writer, drain/execution and cancel/deadline serialization'

# MDEL-01 gates 155-156: API-key activity is an ordinary write. Exercise the
# actual auth capability in both orders; production TouchKey has a matching PG gate.
read -r MT MU MS MR MK <<<"$(mdel_fixture)"
MTOUCH=$(sql <<<"INSERT INTO app.api_keys(tenant_id,issued_by,key_prefix,key_hash) VALUES('$MT','$MU','mdeltouch',sha256(uuidv7()::text::bytea)) RETURNING id;")
gate_close 155
tx mdel_touch_first app_auth "$MT" "SELECT app.auth_touch_api_key('$MTOUCH'); $(park 155)" &
await mdel_touch_first advisory
tx mdel_after_touch postgres "$MT" "$(mdel_call request 0)" &
await mdel_after_touch "${ROWLOCK[@]}"
gate_open 155; wait
expect mdel_touch_first ok; expect mdel_after_touch ok
[[ $(sql <<<"SELECT state||'|'||(SELECT last_used_at IS NOT NULL FROM app.api_keys WHERE id='$MTOUCH') FROM app.tenant_lifecycles WHERE tenant_id='$MT';") == 'cooling_off|true' ]] || { echo 'FAIL: MDEL01_TOUCH_FIRST_RACE'; fail=1; }

read -r MT MU MS MR MK <<<"$(mdel_fixture)"
MTOUCH=$(sql <<<"INSERT INTO app.api_keys(tenant_id,issued_by,key_prefix,key_hash) VALUES('$MT','$MU','mdeltouch',sha256(uuidv7()::text::bytea)) RETURNING id;")
gate_close 156
tx mdel_before_touch postgres "$MT" "$(mdel_call request 0) $(park 156)" &
await mdel_before_touch advisory
tx mdel_touch_late app_auth "$MT" "SELECT app.auth_touch_api_key('$MTOUCH');" &
await mdel_touch_late "${ROWLOCK[@]}"
# Inspect only the waiting touch: the request legitimately holds Identity locks.
# An unlocked key lookup has AccessShareLock; FOR SHARE/UPDATE requires RowShareLock.
[[ $(sql <<<"SELECT NOT EXISTS(SELECT FROM pg_locks l JOIN pg_stat_activity a USING(pid)
 WHERE a.application_name='${RACE_PREFIX}mdel_touch_late' AND l.granted
 AND l.relation IN('app.users'::regclass,'app.tenants'::regclass,'app.tenant_memberships'::regclass,'app.api_keys'::regclass)
 AND l.mode IN('RowShareLock','RowExclusiveLock'));") == t ]] || { echo 'FAIL: MDEL01_TOUCH_LIFECYCLE_LOCK_ORDER'; fail=1; }
gate_open 156; wait
expect mdel_before_touch ok; expect mdel_touch_late 'tenant deletion write fence'
[[ $(sql <<<"SELECT last_used_at IS NULL FROM app.api_keys WHERE id='$MTOUCH';") == t ]] || { echo 'FAIL: MDEL01_TOUCH_AFTER_REQUEST_RACE'; fail=1; }
echo 'PASS: MDEL01 gates 155-156 key activity/request serialization and lifecycle-first lock order'

if [[ $fail -ne 0 ]]; then echo "FAIL: race end state differs from the sealing/reliability/inventory/pricing/shipping/tax/customers/ledger/catalog/cart/orders/notifications/quotes/cms/checkout/customer-auth/fulfillment/returns contract" >&2; exit 1; fi
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
echo 'PASS: CMS gate80 one publisher, gate81 restore ABA epoch fence, gate82 apply/publish lock order'
echo 'PASS: checkout confirm/cancel claims choose one durable winner with atomic order, stock, ledger and outbox witnesses'
# Focused native River/claim/fence gates cannot silently skip for missing PG.
reliability_packages=(./internal/db ./internal/platform/jobs ./internal/platform/idempotency ./internal/platform/outbox)
if [[ -n "${DATABASE_URL:-}" ]]; then
 OPENSITE_DISPOSABLE_PG=1 bash scripts/check_catalog_api.sh
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
