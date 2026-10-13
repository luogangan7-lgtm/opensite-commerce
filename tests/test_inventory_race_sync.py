#!/usr/bin/env python3
"""CI-only real-PG regressions for actual gate readiness and expiry witnesses.

Depends on disposable PostgreSQL and scripts/check_pg_races.sh; called by CI.
An application wait state is not evidence of the exact granted gate lock.
The actual expiry case must survive a controlled worker-startup gap.
"""
import argparse
import os
from pathlib import Path
import signal
import shlex
import subprocess
import tempfile
import time


ROOT = Path(__file__).resolve().parents[1]
GATE = 115


def readiness():
    source = (ROOT / "scripts/check_pg_races.sh").read_text()
    # Include newly added readiness helpers in this region, never a copied implementation.
    helpers = source[source.index("park() {"):source.index("\nROWLOCK=")]
    psql = ["psql", os.environ["DATABASE_URL"], "-X", "-q", "-At",
            "-v", "ON_ERROR_STOP=1"]

    def query(statement):
        result = subprocess.run(psql, input=statement, text=True,
                                capture_output=True, timeout=5)
        if result.returncode:
            raise AssertionError(result.stderr)
        return result.stdout.strip()

    for case in ("ClientRead", "PgSleep", "wrong-key", "wrong-objsubid", "healthy"):
        prefix = f"sync_regression_{os.getpid()}_{case.replace('-', '_')}_"
        name = f"{prefix}gate_{GATE}"
        actor = worker = None
        with tempfile.TemporaryDirectory(prefix="inventory-sync-") as directory:
            start_fifo = Path(directory) / "start"
            os.mkfifo(start_fifo)
            env = dict(os.environ, RACE_PREFIX=prefix, CASE=case,
                       START_FIFO=str(start_fifo), CASE_GATE=str(GATE))
            # For negative cases the launch adapter deliberately withholds the lock.
            # All observer SQL still runs against real pg_stat_activity/pg_locks.
            harness = r'''
set -euo pipefail
fail=0
sql() {
  local statement
  statement=$(cat)
  if [[ $CASE != healthy && $statement == *"SELECT pg_advisory_lock("* && $statement == *"SELECT pg_sleep(600)"* ]]; then
    return 0
  fi
  psql "$DATABASE_URL" -X -q -At -v ON_ERROR_STOP=1 <<<"$statement"
}
''' + helpers + r'''
read -r ready < "$START_FIFO"
if gate_close "$CASE_GATE"; then
  if [[ $CASE != healthy ]]; then
    echo "FAIL: $CASE declared ready without the exact granted driver gate lock"
    exit 1
  fi
  proof=$(sql <<SQL
SELECT count(*)=1 FROM pg_locks l JOIN pg_stat_activity a USING(pid)
WHERE a.application_name='${RACE_PREFIX}gate_$CASE_GATE'
 AND l.locktype='advisory' AND l.mode='ExclusiveLock' AND l.granted
 AND l.classid=$$::oid AND l.objid=$CASE_GATE::oid AND l.objsubid=2;
SQL
)
  [[ $proof == t ]] || { echo 'FAIL: healthy readiness lacks exact lock'; exit 1; }
  gate_open "$CASE_GATE"
else
  [[ $CASE != healthy && $fail == 1 ]] || { echo 'FAIL: unexpected readiness rejection'; exit 1; }
fi
wait
echo "PASS: gate readiness $CASE"
'''
            try:
                worker = subprocess.Popen(["bash", "-c", harness], cwd=ROOT,
                                          env=env, text=True, stdout=subprocess.PIPE,
                                          stderr=subprocess.STDOUT, start_new_session=True)
                if case != "healthy":
                    actor = subprocess.Popen(psql, stdin=subprocess.PIPE,
                                             stdout=subprocess.DEVNULL,
                                             stderr=subprocess.DEVNULL, text=True,
                                             env=dict(os.environ, PGAPPNAME=name))
                    if case == "wrong-key":
                        actor.stdin.write(f"SELECT pg_advisory_lock({worker.pid}, {GATE + 1});\n")
                    elif case == "wrong-objsubid":
                        # Same classid/objid bits, different advisory-lock namespace.
                        actor.stdin.write(f"SELECT pg_advisory_lock({(worker.pid << 32) | GATE}::bigint);\n")
                    if case != "ClientRead":
                        actor.stdin.write("SELECT pg_sleep(120);\n")
                    actor.stdin.flush()
                    expected = "ClientRead" if case == "ClientRead" else "PgSleep"
                    deadline = time.monotonic() + 10
                    while query(f"SELECT coalesce(max(wait_event),'none') FROM pg_stat_activity WHERE application_name='{name}';") != expected:
                        if actor.poll() is not None or time.monotonic() >= deadline:
                            raise AssertionError(f"fixture never reached {expected}")
                        time.sleep(0.02)
                    exact = query(f"SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a USING(pid) WHERE a.application_name='{name}' AND l.locktype='advisory' AND l.mode='ExclusiveLock' AND l.granted AND l.classid={worker.pid}::oid AND l.objid={GATE}::oid AND l.objsubid=2;")
                    if exact != "0":
                        raise AssertionError("negative fixture unexpectedly owns the target lock")
                with start_fifo.open("w") as channel:
                    channel.write("ready\n")
                output, _ = worker.communicate(timeout=45)
                print(output, end="", flush=True)
                if worker.returncode:
                    raise AssertionError(f"gate readiness {case}: exit {worker.returncode}")
            finally:
                if worker is not None and worker.poll() is None:
                    os.killpg(worker.pid, signal.SIGTERM)
                    try:
                        worker.wait(timeout=3)
                    except subprocess.TimeoutExpired:
                        os.killpg(worker.pid, signal.SIGKILL)
                        worker.wait()
                query(f"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE left(application_name,length('{prefix}'))='{prefix}';")
                if actor is not None:
                    if actor.stdin:
                        actor.stdin.close()
                    try:
                        actor.wait(timeout=3)
                    except subprocess.TimeoutExpired:
                        actor.kill()
                        actor.wait()
    print("PASS: actual readiness rejects idle/sleeping impostors and wrong lock namespaces")


def startup():
    source = (ROOT / "scripts/check_pg_races.sh").read_text()
    prelude = source[:source.index("\nfail=0\n")]
    root_line = next(line for line in prelude.splitlines() if line.startswith("ROOT="))
    prelude = prelude.replace(root_line, "ROOT=" + shlex.quote(str(ROOT)), 1)
    fixture_start = source.index("# C-11a inventory:")
    fixtures = source[fixture_start:source.index("\nread -r T SKU OWNER", fixture_start)]
    start = source.index("# Commit owns the reservation")
    block = source[start:source.index("\nread -r T SKU OWNER <<<\"$(inventory_fixture 0)", start)]
    # A controllable scheduler gap: the worker cannot even start BEGIN until
    # the driver makes its original fixture expiry due. No ordering sleep.
    adapter = r'''
fail=0
gate_close 116
eval "$(declare -f tx | sed '1s/tx/original_tx/')"
eval "$(declare -f await | sed '1s/await/original_await/')"
tx() {
  if [[ $1 == inv_commit_wait ]]; then
    original_tx inv_scheduler postgres "$3" "$(park 116)"
  fi
  original_tx "$@"
}
await() {
  if [[ $1 == inv_commit_wait && ${scheduler_released:-0} == 0 ]]; then
    original_await inv_scheduler advisory
    sql <<<"UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id='$T' AND id='$RID';" >/dev/null
    gate_open 116
    scheduler_released=1
  fi
  original_await "$@"
}
'''
    # The Python owner knows the exact participant directory even if Bash is
    # killed before its EXIT trap, and only removes this test's own resource.
    with tempfile.TemporaryDirectory(prefix="inventory-startup-") as directory:
        assert prelude.count('ERRDIR=$(mktemp -d)') == 1, 'participant directory boundary drift'
        prelude = prelude.replace('ERRDIR=$(mktemp -d)', 'ERRDIR=' + shlex.quote(directory), 1)
        script = prelude + fixtures + adapter + block + '\n[[ $fail -eq 0 ]] || exit 1\necho "PASS: inventory expiry witness survives controlled startup gap"\n'
        process = subprocess.Popen(["bash", "-c", script], cwd=ROOT, text=True,
                                   stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            output, _ = process.communicate(timeout=120)
            print(output, end="", flush=True)
            if process.returncode:
                raise AssertionError(f"startup-gap inventory gate: exit {process.returncode}")
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
            # A timed-out Bash signal may bypass its EXIT trap; terminate only this test's sessions.
            subprocess.run(["psql", os.environ["DATABASE_URL"], "-X", "-q", "-v", "ON_ERROR_STOP=1"],
                           input=f"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE left(application_name,length('race_{process.pid}_'))='race_{process.pid}_';",
                           text=True, capture_output=True, timeout=5, check=True)


def main():
    if os.environ.get("GITHUB_ACTIONS") != "true" or not os.environ.get("DATABASE_URL"):
        raise SystemExit("GitHub Actions with a disposable DATABASE_URL is required")
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--case", choices=("readiness", "startup", "all"), default="all")
    args = parser.parse_args()
    failures = []
    for name, test in (("readiness", readiness), ("startup", startup)):
        if args.case in (name, "all"):
            try:
                test()
            except (AssertionError, subprocess.SubprocessError) as error:
                failures.append(name)
                print(f"FAIL: {name}: {error}", flush=True)
    return int(bool(failures))


if __name__ == "__main__":
    raise SystemExit(main())
