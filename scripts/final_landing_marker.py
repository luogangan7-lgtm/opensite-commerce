#!/usr/bin/env python3
"""Decide whether a pull request body requests final landing validation.

Only an exact standalone line consisting of the marker selects final mode.  A
sentence that merely mentions the marker does not: #398's squash commit message
mentioned it in prose, the push-to-main run selected final and failed
FINAL_LANDING_PENDING although no proposal was removed (main run 40403380987).
Commit messages are never consulted; on push the mode comes only from
``scripts/check_landing.py`` trusted reasons and ``FINAL_LANDING_POLICY``.

Reads the PR body from ``FINAL_LANDING_BODY`` and prints ``1`` or ``0`` so the
workflow can hand the value to the landing step as a step output.  Called by
.github/workflows/checks.yml (``foundation`` job) and tests/test_batch_hygiene.py.
Depends on the stdlib only; never talks to GitHub.
"""
import os

MARKER = "[final-landing]"


def requested(body):
    """True only when the marker is a standalone line of the body."""
    return any(line.strip() == MARKER for line in body.splitlines())


def main():
    print("1" if requested(os.environ.get("FINAL_LANDING_BODY", "")) else "0")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
