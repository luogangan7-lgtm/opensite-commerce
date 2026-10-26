#!/usr/bin/env python3
"""Responsibility: secret-step process boundary and allowlisted public receipts.
Dependencies: precompiled CI-only harness; caller: stripe-live-smoke workflow.
Never prints subprocess output, provider exceptions, URLs, email or identifiers.
"""
import json
import os
from pathlib import Path
import re
import subprocess

STATUSES = {'CANCELED', 'REFUNDED', 'TIMED_OUT', 'PENDING',
            'REFUND_UNRESOLVED', 'REFUND_UNRESOLVED_MANUAL'}
FIELDS = {'phase', 'status', 'amount_minor', 'currency', 'livemode', 'owner_action_needed'}


def public_summary(value):
    if not isinstance(value, dict) or set(value) not in (FIELDS, FIELDS | {'counts'}):
        raise ValueError('invalid summary')
    if value['phase'] not in ('A', 'B', 'R') or value['status'] not in STATUSES:
        raise ValueError('invalid state')
    if type(value['amount_minor']) is not int or value['amount_minor'] not in ((50,) if value['phase'] == 'A' else ((50, 5000) if value['phase'] == 'R' else (5000,))):
        raise ValueError('invalid amount')
    if value['currency'] != 'USD' or value['livemode'] is not True or type(value['owner_action_needed']) is not bool:
        raise ValueError('invalid projection')
    if value['owner_action_needed'] != (value['status'] in {'REFUND_UNRESOLVED', 'REFUND_UNRESOLVED_MANUAL'}):
        raise ValueError('missing owner-action state')
    if 'counts' in value:
        counts = value['counts']
        if not isinstance(counts, dict) or set(counts) != {'capture_journals', 'refund_journals', 'extra_replay_journals'} or any(type(n) is not int or n not in (0, 1) for n in counts.values()):
            raise ValueError('invalid counts')
    return json.dumps(value, sort_keys=True)


def main():
    # Fail before spawning any consumer if the execution/source/input is wrong.
    run_id = os.environ.get('STRIPE_SMOKE_RUN_ID', '')
    current = os.environ.get('GITHUB_RUN_ID', '')
    sha = os.environ.get('GITHUB_SHA', '')
    phase = os.environ.get('STRIPE_SMOKE_PHASE', '')
    attempt = os.environ.get('STRIPE_REFUND_ATTEMPT', '')
    if (os.environ.get('GITHUB_ACTIONS') != 'true'
        or os.environ.get('GITHUB_EVENT_NAME') != 'workflow_dispatch'
        or os.environ.get('GITHUB_REF') != 'refs/heads/main'
        or not re.fullmatch(r'[0-9]{1,32}', run_id)
        or not re.fullmatch(r'[0-9]{1,32}', current)
        or not re.fullmatch(r'[0-9a-f]{40}', sha)
        or phase not in ('A', 'B', 'R')
        or (phase == 'R' and os.environ.get('STRIPE_SMOKE_HAS_ORIGINAL_RUN_ID') != 'true')
        or (attempt and (phase != 'R' or not re.fullmatch(r'[0-9]{1,9}', attempt) or int(attempt) < 2))):
        raise SystemExit('PENDING: invalid owner-gated execution; no provider details')
    try:
        receipt = f'original_run={run_id} current_run={current} source_sha={sha}'
        print(receipt, flush=True)
        with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as target:
            target.write(receipt + '\n')
        result = subprocess.run([str(Path(os.environ['RUNNER_TEMP']) / 'stripe-live.test'),
                                 '-test.run=^TestStripeLiveOwnerGate$', '-test.timeout=35m'],
                                cwd=Path(__file__).resolve().parents[1] / 'internal/db',
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                text=True, timeout=2110, check=False)
        values = [json.loads(line.removeprefix('OSC_LIVE_SUMMARY '))
                  for line in result.stdout.splitlines() if line.startswith('OSC_LIVE_SUMMARY ')]
        if len(values) != 1:
            raise ValueError('missing projection')
        summary = public_summary(values[0])
        print(summary, flush=True)
        owner_action = '\nowner action needed: reimburse and retain evidence; smoke remains failed' if values[0]['owner_action_needed'] else ''
        with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as target:
            target.write(f'{summary}\n\noriginal_run={run_id} current_run={current} source_sha={sha}{owner_action}\n')
        if result.returncode:
            raise SystemExit('PENDING: smoke failed; re-enter R with original_run; private diagnostics discarded')
    except Exception:
        # Never interpolate an exception or captured output into a public sink.
        raise SystemExit('PENDING: smoke unresolved; re-enter R with original_run; private diagnostics discarded') from None


if __name__ == '__main__':
    main()
