"""Re-run attempt selection shared by the landing gate and the golden verifier.

Responsibility: given GitHub job results for one run and the receipt/artifact
attempts that exist, decide which attempt is authoritative.  A run may have
several attempts: a failed-job re-run only re-executes failed jobs, while a full
re-run succeeds again for every job.  For one lane the authoritative attempt is
the latest attempt GitHub reports as a ``completed`` job with conclusion
``success``.  That exact attempt must have its own receipt/artifact; a missing
newer receipt fails closed and an older attempt is never substituted.

Depends on: the Python standard library only.
Called by: scripts/check_landing.py (lane identity receipts) and
tests/test_batch_hygiene.py.  scripts/frontend/rerun-attempts.ts implements the
identical rule for the producer verifier because the two consumers run in
different languages; both suites replay the same vectors from
tests/fixtures/rerun-attempts.json, which is the cross-language contract.
Input records come from GitHub's
``actions/runs/<id>/jobs?filter=all`` (paginated by the caller), never from the
default listing that only shows the latest execution.
"""


import re


class AttemptError(ValueError):
    """The authoritative attempt cannot be selected without guessing."""


def attempt_number(value):
    """Normalize a GitHub ``run_attempt`` to a positive int, or fail closed."""
    text = "" if value is None else str(value).strip()
    if not re.fullmatch(r"[1-9][0-9]*", text):
        raise AttemptError(f"run_attempt {value!r} is not a positive integer")
    return int(text)


def successful_attempts(jobs, job_name):
    """Ascending attempts where ``job_name`` completed successfully."""
    attempts = set()
    for job in jobs:
        if job.get("name") != job_name:
            continue
        # A malformed attempt on a matching job is a broken listing, not a
        # reason to skip the lane, so it fails closed here.
        attempt = attempt_number(job.get("run_attempt"))
        if job.get("status") == "completed" and job.get("conclusion") == "success":
            attempts.add(attempt)
    return sorted(attempts)


def select_latest_attempt(successful, evidence):
    """The latest successful attempt, which must itself have evidence."""
    attempts = [attempt_number(attempt) for attempt in successful]
    if not attempts:
        raise AttemptError("no successful attempt")
    latest = max(attempts)
    available = sorted({attempt_number(attempt) for attempt in evidence})
    if latest not in available:
        listing = ", ".join(str(attempt) for attempt in available) or "none"
        raise AttemptError(
            f"latest successful attempt {latest} has no receipt/artifact evidence "
            f"(available attempt(s): {listing}); an older attempt is never accepted")
    return latest


def latest_successful_attempt(jobs, job_name):
    """Latest successful attempt for one job name (evidence is the job itself)."""
    attempts = successful_attempts(jobs, job_name)
    return select_latest_attempt(attempts, attempts)
