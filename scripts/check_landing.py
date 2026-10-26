#!/usr/bin/env python3
"""Proposal validation and final migration landing are separate CI decisions.

Proposal mode validates that every ``migrations/pending`` entry is orderable and
is what the ephemeral CI lanes materialize.  It deliberately does not claim that
in-development proposals are ready to land on ``main``.

Final mode is the landing gate: ``migrations/pending`` must be empty, numbered
history must stay contiguous ``001..N``, the receipt must bind the exact head
SHA, and every prerequisite lane must have independently recorded that it
checked out that same head (not the synthetic ``pull_request`` merge commit).

Mode selection is mandatory and fail-closed.  ``auto`` derives final landing
from trusted repository changes (a ``git diff --name-status`` listing produced
by CI: a removed proposal or any change to numbered history) or from a trusted
policy value (``FINAL_LANDING_POLICY`` repository variable).  The optional
``[final-landing]`` marker may only add final validation; it can never select
proposal mode, so omitting it cannot bypass the zero-pending requirement.  An
``auto`` run with neither input fails closed instead of defaulting to proposed.

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


def validate_lane_identities(directory, expected_sha, lanes, run_id, run_attempt):
    """Every prerequisite lane must have recorded the exact checkout it tested."""
    if directory is None:
        raise LandingError(
            "FINAL_LANDING_LANE_IDENTITIES: final mode requires --lane-identities "
            "(one receipt per prerequisite lane)")
    if not directory.is_dir():
        raise LandingError(f"FINAL_LANDING_LANE_IDENTITIES: {directory} is not a directory")
    identities, seen = {}, {}
    for path in sorted(directory.glob("*.json")):
        try:
            payload = json.loads(path.read_text())
        except (OSError, ValueError) as error:
            raise LandingError(f"FINAL_LANDING_LANE_IDENTITIES: {path.name}: {error}")
        if not isinstance(payload, dict):
            raise LandingError(f"FINAL_LANDING_LANE_IDENTITIES: {path.name}: not a JSON object")
        lane = payload.get("lane")
        if lane not in lanes:
            raise LandingError(f"FINAL_LANDING_LANE_IDENTITIES: {path.name}: unexpected lane {lane!r}")
        if lane in identities:
            raise LandingError(f"FINAL_LANDING_LANE_IDENTITIES: duplicate receipt for lane {lane}")
        if payload.get("checked_out_sha") != expected_sha:
            raise LandingError(
                f"FINAL_LANDING_LANE_IDENTITY: {lane} checked out {payload.get('checked_out_sha')!r}, "
                f"not the exact head {expected_sha}")
        if payload.get("event_sha") != expected_sha:
            raise LandingError(
                f"FINAL_LANDING_LANE_IDENTITY: {lane} recorded event head {payload.get('event_sha')!r}, "
                f"not the exact head {expected_sha}")
        if run_id and payload.get("run_id") != run_id:
            raise LandingError(
                f"FINAL_LANDING_LANE_IDENTITY: {lane} ran in run {payload.get('run_id')!r}, not {run_id}")
        if run_attempt and payload.get("run_attempt") != run_attempt:
            raise LandingError(
                f"FINAL_LANDING_LANE_IDENTITY: {lane} ran in attempt {payload.get('run_attempt')!r}, "
                f"not {run_attempt}")
        identities[lane] = {key: payload.get(key) for key in LANE_IDENTITY_KEYS}
    missing = [lane for lane in lanes if lane not in identities]
    if missing:
        raise LandingError(
            f"FINAL_LANDING_LANE_IDENTITIES: missing lane receipt(s): {', '.join(missing)}")
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
                os.environ.get("GITHUB_RUN_ID", ""), os.environ.get("GITHUB_RUN_ATTEMPT", ""))
        else:
            pending, ordered = validate_proposal(root)
            numbered = pending_migrations.numbered(root)
            identities = (validate_lane_identities(
                args.lane_identities, expected, lanes,
                os.environ.get("GITHUB_RUN_ID", ""), os.environ.get("GITHUB_RUN_ATTEMPT", ""))
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
