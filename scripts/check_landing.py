#!/usr/bin/env python3
"""Proposal validation and final migration landing are separate CI decisions.

Proposal mode validates that every ``migrations/pending`` entry is orderable and
is what the ephemeral CI lanes materialize.  It deliberately does not claim that
in-development proposals are ready to land on ``main``.

Final mode is the landing gate: ``migrations/pending`` must be empty, numbered
history must stay contiguous ``001..N``, the receipt must bind the exact head
SHA, and every prerequisite lane must have independently recorded that it
checked out that same head (not the synthetic ``pull_request`` merge commit).

A run may have several attempts: re-running failed jobs only re-executes the
matrix jobs that failed, so a lane that passed earlier keeps that attempt's
receipt while the re-run lane uploads a new one.  Lane receipts are therefore
collected across attempts, and per lane the latest attempt that GitHub reports
as a successful ``foundation-<lane>`` job of this run at the exact head SHA is
accepted.  The job results (``actions/runs/<id>/jobs?filter=all``) are required
so that the ``if: always()`` receipt uploaded by a failed attempt can never be
mistaken for success; missing job results, a lane without a successful job, or
a lane without a matching successful receipt all fail closed.

Mode selection is mandatory and fail-closed.  ``auto`` derives final landing
from trusted repository changes (a ``git diff --name-status`` listing produced
by CI: a removed proposal or any change to numbered history) or from a trusted
policy value (``FINAL_LANDING_POLICY`` repository variable).  The optional
``[final-landing]`` marker may only add final validation; it can never select
proposal mode, so omitting it cannot bypass the zero-pending requirement.  It is
honored only as an exact standalone line of a pull request body
(``scripts/final_landing_marker.py``): a commit message or prose that merely
mentions the marker never selects final.  An ``auto`` run with neither input
fails closed instead of defaulting to proposed.

Called by .github/workflows/checks.yml (``final-landing`` step of the required
``foundation`` job) and tests/test_batch_hygiene.py.  Depends on
scripts/pending_migrations.py and the stdlib only; never connects to
PostgreSQL.
"""
import argparse
import json
import os
import re
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import pending_migrations  # noqa: E402

LANES = ("go", "offline", "pg", "pgx", "sqlc")
LANE_JOB_PREFIX = "foundation-"
PENDING_PREFIX = "migrations/pending/"
NUMBERED = re.compile(r"migrations/[0-9]{3}_[a-z][a-z0-9_]*\.sql")
# `git diff --name-status` codes that mean numbered history changed at all.
NUMBERED_CHANGE = {"A", "C", "D", "M", "R", "T"}
# Codes that mean a pending proposal is no longer pending (landing or removal).
PENDING_REMOVAL = {"D", "R"}
LANE_IDENTITY_KEYS = ("lane", "event_sha", "checked_out_sha", "run_id", "run_attempt")
TRUSTED_POLICY_VALUES = {"1", "true", "yes", "on", "final"}


class LandingError(Exception):
    pass


def pending_files(root):
    directory = root / "migrations" / "pending"
    if not directory.is_dir():
        return []
    return sorted(path for path in directory.iterdir() if path.is_file())


def validate_proposal(root):
    pending = pending_files(root)
    # Unknown deps, cycles, duplicate ids/names and bad headers fail closed here,
    # before any lane materializes the proposals.
    ordered = pending_migrations.ordered_pending(root)
    return pending, ordered


def validate_final(root, sha, expected_sha):
    pending = pending_files(root)
    if pending:
        names = ", ".join(path.name for path in pending)
        raise LandingError(f"FINAL_LANDING_PENDING: {len(pending)} proposal(s) remain: {names}")
    numbered = pending_migrations.numbered(root)
    if not sha:
        raise LandingError("FINAL_LANDING_SHA_MISSING: final mode requires --sha")
    if not expected_sha:
        raise LandingError("FINAL_LANDING_SHA_MISSING: final mode requires --expected-sha")
    if sha != expected_sha:
        raise LandingError(
            f"FINAL_LANDING_SHA_MISMATCH: checked-out {sha} is not the exact head {expected_sha}")
    return pending, numbered


def parse_changed(text):
    """Parse `git diff --name-status` output into [(status, [paths...])]."""
    rows = []
    for lineno, raw in enumerate(text.splitlines(), 1):
        line = raw.rstrip("\n")
        if not line.strip():
            continue
        fields = line.split("\t")
        status, paths = fields[0].strip(), [field for field in fields[1:] if field]
        if not paths:
            raise LandingError(f"FINAL_LANDING_DECISION_UNKNOWN: malformed diff line {lineno}: {raw!r}")
        rows.append((status[:1], paths))
    return rows


def landing_reasons(rows):
    """Why trusted repository changes force final landing validation."""
    reasons = []
    for status, paths in rows:
        for path in paths:
            if path.startswith(PENDING_PREFIX) and status in PENDING_REMOVAL:
                reasons.append(f"proposal removed: {path}")
            elif NUMBERED.fullmatch(path) and status in NUMBERED_CHANGE:
                reasons.append(f"numbered migration changed: {path}")
    return reasons


def policy_final():
    """Trusted policy only: a repository variable, never PR-controlled text."""
    value = os.environ.get("FINAL_LANDING_POLICY", "").strip().lower()
    return value in TRUSTED_POLICY_VALUES


def select_mode(requested, rows, final_policy):
    reasons = landing_reasons(rows)
    if requested == "proposal" and (reasons or final_policy):
        detail = "; ".join(reasons) or "trusted landing policy"
        raise LandingError(
            "FINAL_LANDING_REQUIRED: repository changes select landing validation "
            f"({detail}); proposal mode cannot discharge it")
    if requested == "final":
        return "final", reasons
    if reasons or final_policy:
        return "final", reasons
    return "proposal", reasons


def load_lane_jobs(path):
    """GitHub's job results for this run, one JSON object per line (or an array).

    CI writes the output of
    ``repos/{repo}/actions/runs/{run_id}/jobs?filter=all``; accepting a JSON
    array as well keeps the gate exercisable offline.  The results are the only
    evidence that distinguishes a successful attempt from the always-uploaded
    receipt of a failed one, so an absent or empty listing fails closed.
    """
    if path is None:
        raise LandingError(
            "FINAL_LANDING_LANE_JOBS: lane identity validation requires --lane-jobs "
            "(GitHub job results for this run across every attempt)")
    if not path.is_file():
        raise LandingError(f"FINAL_LANDING_LANE_JOBS: {path} is not a file")
    try:
        text = path.read_text()
        if text.lstrip().startswith("["):
            jobs = json.loads(text)
        else:
            jobs = [json.loads(line) for line in text.splitlines() if line.strip()]
    except (OSError, ValueError) as error:
        raise LandingError(f"FINAL_LANDING_LANE_JOBS: {path.name}: {error}")
    if not isinstance(jobs, list) or not all(isinstance(job, dict) for job in jobs):
        raise LandingError(f"FINAL_LANDING_LANE_JOBS: {path.name}: expected job objects")
    if not jobs:
        raise LandingError(f"FINAL_LANDING_LANE_JOBS: {path.name}: no job results")
    return jobs


def successful_job_attempts(jobs, lanes, run_id, expected_sha):
    """Per lane, the attempts GitHub recorded as a successful job of this run.

    Every matching job must belong to this run and to the exact head SHA: the
    run's ``head_sha`` is the checkout the workflow was started for, so it
    binds the accepted receipts to the same commit the gate is validating.
    Returns ``(successful, recorded)`` so a failure can name the attempts that
    did run, including ones that did not succeed.
    """
    succeeded = {lane: [] for lane in lanes}
    recorded = {lane: [] for lane in lanes}
    for job in jobs:
        name = job.get("name")
        if not isinstance(name, str) or not name.startswith(LANE_JOB_PREFIX):
            continue
        lane = name[len(LANE_JOB_PREFIX):]
        if lane not in succeeded:
            continue
        if run_id and str(job.get("run_id")) != str(run_id):
            raise LandingError(
                "FINAL_LANDING_LANE_JOBS: "
                f"{lane} job belongs to run {job.get('run_id')!r}, not {run_id}")
        if job.get("head_sha") != expected_sha:
            raise LandingError(
                "FINAL_LANDING_LANE_JOBS: "
                f"{lane} job ran at {job.get('head_sha')!r}, not the exact head {expected_sha}")
        attempt = job.get("run_attempt")
        if not str(attempt or "").isdigit():
            raise LandingError(
                f"FINAL_LANDING_LANE_JOBS: {lane} job has no numeric run_attempt")
        attempt = str(attempt)
        recorded[lane].append(attempt)
        if job.get("status") == "completed" and job.get("conclusion") == "success":
            succeeded[lane].append(attempt)
    return succeeded, recorded


def validate_lane_identities(directory, expected_sha, lanes, run_id, lane_jobs):
    """Every prerequisite lane must have recorded the exact checkout it tested.

    Receipts may span attempts because a failed-job re-run leaves earlier
    attempts' artifacts in place.  Per lane the latest receipt whose attempt
    GitHub recorded as a successful job of this run at the exact head SHA is
    accepted; a foreign run, a different SHA, an unsuccessful attempt, a
    duplicate receipt for one attempt, or a missing receipt fails closed.
    """
    if directory is None:
        raise LandingError(
            "FINAL_LANDING_LANE_IDENTITIES: final mode requires --lane-identities "
            "(one receipt per prerequisite lane)")
    if not directory.is_dir():
        raise LandingError(f"FINAL_LANDING_LANE_IDENTITIES: {directory} is not a directory")
    succeeded, recorded = successful_job_attempts(
        load_lane_jobs(lane_jobs), lanes, run_id, expected_sha)
    candidates, seen = {}, set()
    for path in sorted(directory.rglob("*.json")):
        try:
            payload = json.loads(path.read_text())
        except (OSError, ValueError) as error:
            raise LandingError(f"FINAL_LANDING_LANE_IDENTITIES: {path.name}: {error}")
        if not isinstance(payload, dict):
            raise LandingError(f"FINAL_LANDING_LANE_IDENTITIES: {path.name}: not a JSON object")
        lane = payload.get("lane")
        if lane not in lanes:
            raise LandingError(f"FINAL_LANDING_LANE_IDENTITIES: {path.name}: unexpected lane {lane!r}")
        attempt = payload.get("run_attempt")
        if not str(attempt or "").isdigit():
            raise LandingError(
                f"FINAL_LANDING_LANE_IDENTITIES: {path.name}: missing numeric run_attempt")
        attempt = str(attempt)
        if (lane, attempt) in seen:
            raise LandingError(
                f"FINAL_LANDING_LANE_IDENTITIES: duplicate receipt for lane {lane} "
                f"in attempt {attempt}")
        seen.add((lane, attempt))
        if payload.get("checked_out_sha") != expected_sha:
            raise LandingError(
                f"FINAL_LANDING_LANE_IDENTITY: {lane} checked out {payload.get('checked_out_sha')!r}, "
                f"not the exact head {expected_sha}")
        if payload.get("event_sha") != expected_sha:
            raise LandingError(
                f"FINAL_LANDING_LANE_IDENTITY: {lane} recorded event head {payload.get('event_sha')!r}, "
                f"not the exact head {expected_sha}")
        if run_id and str(payload.get("run_id")) != str(run_id):
            raise LandingError(
                f"FINAL_LANDING_LANE_IDENTITY: {lane} receipt is from run {payload.get('run_id')!r}, "
                f"not {run_id}")
        candidates.setdefault(lane, []).append((attempt, payload))
    problems = []
    for lane in lanes:
        receipts = candidates.get(lane, [])
        if not succeeded[lane]:
            attempts = ", ".join(sorted(set(recorded[lane]))) or "none"
            problems.append(
                f"{lane}: no successful job result for run {run_id or '<unset>'} at "
                f"{expected_sha} (recorded attempt(s): {attempts})")
            continue
        successful_attempts = set(succeeded[lane])
        if not any(attempt in successful_attempts for attempt, _ in receipts):
            seen_attempts = ", ".join(sorted(attempt for attempt, _ in receipts)) or "none"
            problems.append(
                f"{lane}: no successful receipt (receipt attempt(s): {seen_attempts}; "
                f"successful attempt(s): {', '.join(sorted(successful_attempts))})")
    if problems:
        raise LandingError("FINAL_LANDING_LANE_IDENTITIES: " + "; ".join(problems))
    identities = {}
    for lane in lanes:
        successful_attempts = set(succeeded[lane])
        attempt, payload = max(
            ((attempt, payload) for attempt, payload in candidates[lane]
             if attempt in successful_attempts),
            key=lambda row: int(row[0]))
        identities[lane] = {key: payload.get(key) for key in LANE_IDENTITY_KEYS}
    return [identities[lane] for lane in lanes]


def receipt(root, mode, sha, pending, numbered, reasons, final_policy, lanes):
    return {
        "mode": mode,
        "source_sha": sha,
        "pending": len(pending),
        "numbered": len(numbered),
        "landing_reasons": reasons,
        "policy_final": final_policy,
        "lane_identities": lanes,
        "repository": os.environ.get("GITHUB_REPOSITORY", ""),
        "workflow": os.environ.get("GITHUB_WORKFLOW", ""),
        "event": os.environ.get("GITHUB_EVENT_NAME", ""),
        "run_id": os.environ.get("GITHUB_RUN_ID", ""),
        "run_attempt": os.environ.get("GITHUB_RUN_ATTEMPT", ""),
        "root": str(root),
    }


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=HERE.parent)
    parser.add_argument("--mode", choices=("auto", "proposal", "final"), default="auto")
    parser.add_argument("--sha", default=os.environ.get("GITHUB_SHA", ""))
    parser.add_argument("--expected-sha", default="")
    parser.add_argument("--changed-paths", type=Path,
                        help="trusted `git diff --name-status` listing for migrations/")
    parser.add_argument("--lane-identities", type=Path,
                        help="directory of per-lane checkout identity receipts")
    parser.add_argument("--lane-jobs", type=Path,
                        help="GitHub job results for this run across attempts "
                             "(JSON lines or an array)")
    parser.add_argument("--lanes", default=",".join(LANES))
    parser.add_argument("--receipt", type=Path)
    args = parser.parse_args(argv)
    root = args.root.resolve()
    expected = args.expected_sha or args.sha
    final_policy = policy_final() or os.environ.get("FINAL_LANDING") == "1"
    lanes = tuple(lane.strip() for lane in args.lanes.split(",") if lane.strip())
    try:
        if args.mode == "auto" and args.changed_paths is None and not final_policy:
            raise LandingError(
                "FINAL_LANDING_DECISION_UNKNOWN: --mode auto needs --changed-paths from the "
                "trusted diff or trusted policy; refusing to default to proposal")
        rows = parse_changed(args.changed_paths.read_text()) if args.changed_paths else []
        mode, reasons = select_mode(args.mode, rows, final_policy)
        if mode == "final":
            pending, numbered = validate_final(root, args.sha, expected)
            identities = validate_lane_identities(
                args.lane_identities, expected, lanes,
                os.environ.get("GITHUB_RUN_ID", ""), args.lane_jobs)
        else:
            pending, ordered = validate_proposal(root)
            numbered = pending_migrations.numbered(root)
            identities = (validate_lane_identities(
                args.lane_identities, expected, lanes,
                os.environ.get("GITHUB_RUN_ID", ""), args.lane_jobs)
                if args.lane_identities else [])
    except (LandingError, pending_migrations.PendingError) as error:
        print(f"{error}", file=sys.stderr)
        return 2
    payload = receipt(root, mode, args.sha, pending, numbered, reasons, final_policy, identities)
    if args.receipt:
        args.receipt.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n")
    print(json.dumps(payload, sort_keys=True))
    if mode == "final":
        print(f"FINAL LANDING OK: exact source {args.sha}, {len(numbered)} contiguous migrations, "
              f"0 pending, {len(identities)} lane identities")
    else:
        print(f"PROPOSAL MODE: {len(pending)} pending migration(s); final landing not claimed")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
