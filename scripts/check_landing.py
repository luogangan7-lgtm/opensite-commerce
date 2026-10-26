#!/usr/bin/env python3
"""Proposal validation and final migration landing are separate CI decisions.

Proposal mode validates that every ``migrations/pending`` entry is orderable and
is what the ephemeral CI lanes materialize.  It deliberately does not claim that
in-development proposals are ready to land on ``main``.

Final mode is the landing gate: ``migrations/pending`` must be empty, numbered
history must stay contiguous ``001..N``, and the receipt must bind the exact
head SHA supplied by CI (never the synthetic ``pull_request`` merge commit).
An optional marker ``[final-landing]`` in the landing PR body or head commit
selects final mode; ordinary proposal PRs stay in proposal mode.

Called by .github/workflows/checks.yml (``final-landing`` job) and
tests/test_batch_hygiene.py.  Depends on scripts/pending_migrations.py and the
stdlib only; never connects to PostgreSQL.
"""
import argparse
import json
import os
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import pending_migrations  # noqa: E402


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


def receipt(root, mode, sha, pending, numbered):
    return {
        "mode": mode,
        "source_sha": sha,
        "pending": len(pending),
        "numbered": len(numbered),
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
    parser.add_argument("--receipt", type=Path)
    args = parser.parse_args(argv)
    root = args.root.resolve()
    expected = args.expected_sha or args.sha
    mode = args.mode
    if mode == "auto":
        mode = "final" if os.environ.get("FINAL_LANDING") == "1" else "proposal"
    try:
        if mode == "final":
            pending, numbered = validate_final(root, args.sha, expected)
        else:
            pending, ordered = validate_proposal(root)
            numbered = pending_migrations.numbered(root)
    except (LandingError, pending_migrations.PendingError) as error:
        print(f"{error}", file=sys.stderr)
        return 2
    payload = receipt(root, mode, args.sha, pending, numbered)
    if args.receipt:
        args.receipt.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n")
    print(json.dumps(payload, sort_keys=True))
    if mode == "final":
        print(f"FINAL LANDING OK: exact source {args.sha}, {len(numbered)} contiguous migrations, 0 pending")
    else:
        print(f"PROPOSAL MODE: {len(pending)} pending migration(s); final landing not claimed")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
