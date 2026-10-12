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
# $RACE_PREFIX<label> for the driver. Never aborts the gate; output kept per label.
RACE_PREFIX="race_$$_"
ERRDIR=$(mktemp -d)
cleanup() {  # never leave parked participants or gate sessions behind
  sql <<<"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE left(application_name,length('$RACE_PREFIX'))='$RACE_PREFIX';" >/dev/null 2>&1 || true
  rm -rf "$ERRDIR"
}
trap cleanup EXIT
tx() {
  printf "SET application_name='${RACE_PREFIX}%s'; BEGIN; SELECT set_config('app.tenant_id','%s',true); SET LOCAL ROLE %s; %s COMMIT;\n" \
    "$1" "$3" "$2" "$4" | sql >"$ERRDIR/$1.out" 2>"$ERRDIR/$1" || true
}
park() { echo "SELECT pg_advisory_xact_lock_shared($$, $1);"; } # SQL: wait for gate $1
gate_close() {  # driver session holds advisory lock $1 until gate_open
  echo "SET application_name='${RACE_PREFIX}gate_$1'; SELECT pg_advisory_lock($$, $1); SELECT pg_sleep(600);" | sql >/dev/null 2>&1 &
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
  echo "  sync timeout: ${RACE_PREFIX}$label never reached ${*} (last: $ev)"; fail=1; return 1
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
if [[ $fail -ne 0 ]]; then echo "FAIL: race end state differs from the sealing/reliability contract" >&2; exit 1; fi
echo 'PASS: lock-synchronized sealing, disjoint outbox claims, lease fencing and same-key occupancy'
