#!/usr/bin/env python3
"""CI-only inventory soak; executes the unchanged inventory block of the race gate.

Depends on the migrated disposable PG18 database and check_pg_races.sh. Called
by inventory-race-soak.yml; every trial counts, failures are never retried, and
full participant output/lock witnesses survive the gate's normal cleanup.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shlex
import signal
import subprocess
import time

ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "scripts/check_pg_races.sh"


def focused_source():
    source = SOURCE.read_text()
    start = "# C-11a inventory:"
    end = "# Pricing's unique tier"
    if source.count(start) != 1 or source.count(end) != 1:
        raise ValueError("inventory gate boundaries changed; refusing an empty/partial soak")
    prelude = source[:source.index("\nfail=0\n")]
    root_line = next(line for line in prelude.splitlines() if line.startswith("ROOT="))
    prelude = prelude.replace(root_line, "ROOT=" + shlex.quote(str(ROOT)), 1)
    block = source[source.index(start):source.index(end)]
    diagnostics = r'''
fail=0
diagnostics() {
  echo '=== failure: complete participant stdout/stderr ==='
  for file in "$ERRDIR"/*; do
    [[ -f $file ]] || continue
    echo "=== $(basename "$file") ==="; cat "$file"
  done
  echo '=== failure: current participant activity/locks ==='
  sql <<SQL || true
SELECT pid,application_name,state,wait_event_type,wait_event,xact_start,query
 FROM pg_stat_activity WHERE left(application_name,length('$RACE_PREFIX'))='$RACE_PREFIX' ORDER BY pid;
SELECT a.application_name,l.locktype,l.mode,l.granted,l.classid,l.objid,l.objsubid,
 l.relation::regclass,l.transactionid,pg_blocking_pids(a.pid)
 FROM pg_locks l JOIN pg_stat_activity a USING(pid)
 WHERE left(a.application_name,length('$RACE_PREFIX'))='$RACE_PREFIX'
 ORDER BY a.application_name,l.locktype,l.granted;
SQL
  if [[ ${T:-} =~ ^[0-9a-f-]{36}$ ]]; then
    sql <<SQL || true
SELECT clock_timestamp() AS observed_clock,id,state,quantity,expires_at,finished_at
 FROM app.inventory_reservations WHERE tenant_id='$T' ORDER BY id;
SELECT tenant_id,warehouse_id,sku_id,on_hand,reserved
 FROM app.stock_levels WHERE tenant_id='$T' ORDER BY warehouse_id,sku_id;
SQL
  fi
}
trap 'status=$?; if [[ $status -ne 0 || $fail -ne 0 ]]; then diagnostics; fi; cleanup' EXIT
trap 'exit 143' TERM INT
'''
    return prelude + diagnostics + block + '\n[[ $fail -eq 0 ]] || exit 1\necho "PASS: focused inventory races"\n'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--trials", type=int, default=50)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if os.environ.get("GITHUB_ACTIONS") != "true" or not os.environ.get("DATABASE_URL"):
        parser.error("GitHub Actions with a disposable DATABASE_URL is required")
    if not 1 <= args.trials <= 50:
        parser.error("trials must be in 1..50")
    args.output.mkdir(parents=True, exist_ok=True)
    script = focused_source()
    (args.output / "executed-gates.sh").write_text(script)
    receipt = {"source_sha": os.environ.get("GITHUB_SHA"),
               "run_id": os.environ.get("GITHUB_RUN_ID"), "requested_trials": args.trials,
               "race_source_sha256": hashlib.sha256(SOURCE.read_bytes()).hexdigest(),
               "executed_source_sha256": hashlib.sha256(script.encode()).hexdigest(),
               "trials": []}
    for trial in range(1, args.trials + 1):
        log = args.output / f"trial-{trial:02d}.log"
        started = time.monotonic()
        with log.open("wb") as output:
            process = subprocess.Popen(["bash", "-c", script], cwd=ROOT,
                                       stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
            try:
                code = process.wait(timeout=180)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
                code = 124
                output.write(b"\nFAIL: trial exceeded 180s; no retry\n")
        entry = {"trial": trial, "exit_code": code, "seconds": round(time.monotonic() - started, 3),
                 "log": log.name, "sha256": hashlib.sha256(log.read_bytes()).hexdigest()}
        receipt["trials"].append(entry)
        failures = sum(row["exit_code"] != 0 for row in receipt["trials"])
        receipt.update(completed_trials=trial, failures=failures, failure_rate=failures / trial)
        (args.output / "summary.json").write_text(json.dumps(receipt, indent=2) + "\n")
        print(f"TRIAL {trial}/{args.trials}: exit={code}, seconds={entry['seconds']}, log={log.name}", flush=True)
    print(f"SOAK: {receipt['failures']}/{args.trials} failures", flush=True)
    return int(receipt["failures"] != 0)


if __name__ == "__main__":
    raise SystemExit(main())
