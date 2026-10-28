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

STATUSES = {'CANCELED', 'PENDING'}
FIELDS = {'phase', 'status', 'amount_minor', 'currency', 'livemode', 'owner_action_needed'}


def public_summary(value):
    if not isinstance(value, dict) or set(value) not in (FIELDS, FIELDS | {'counts'}):
        raise ValueError('invalid summary')
    if value['phase'] != 'A' or value['status'] not in STATUSES:
        raise ValueError('invalid state')
    if type(value['amount_minor']) is not int or value['amount_minor'] != 50:
        raise ValueError('invalid amount')
    if value['currency'] != 'USD' or value['livemode'] is not True or type(value['owner_action_needed']) is not bool:
        raise ValueError('invalid projection')
    if value['owner_action_needed'] is not False:
        raise ValueError('missing owner-action state')
    if 'counts' in value:
        counts = value['counts']
        if not isinstance(counts, dict) or set(counts) != {'capture_journals', 'refund_journals', 'extra_replay_journals'} or any(type(n) is not int or n != 0 for n in counts.values()):
            raise ValueError('invalid counts')
    return json.dumps(value, sort_keys=True)


def main():
    # Fail before spawning any consumer if the execution/source/input is wrong.
    run_id = os.environ.get('STRIPE_SMOKE_RUN_ID', '')
    current = os.environ.get('GITHUB_RUN_ID', '')
    sha = os.environ.get('GITHUB_SHA', '')
    phase = os.environ.get('STRIPE_SMOKE_PHASE', '')
    if (os.environ.get('GITHUB_ACTIONS') != 'true'
        or os.environ.get('GITHUB_EVENT_NAME') != 'workflow_dispatch'
        or os.environ.get('GITHUB_REF') != 'refs/heads/main'
        or not re.fullmatch(r'[0-9]{1,32}', run_id)
        or not re.fullmatch(r'[0-9]{1,32}', current)
        or not re.fullmatch(r'[0-9a-f]{40}', sha)
        or phase != 'A'
        or run_id != current
        or os.environ.get('GITHUB_RUN_ATTEMPT') != '1'):
        raise SystemExit('PENDING: invalid owner-gated execution; no provider details')
    account = os.environ.get('STRIPE_SMOKE_ACCOUNT', '')
    if len(account) > 128 or not re.fullmatch(r'acct_[A-Za-z0-9_-]+', account):
        raise SystemExit('PENDING: OWNER ACTION: add Environment secret STRIPE_SMOKE_ACCOUNT=acct_... in stripe-live (expected account); no provider request made')
    try:
        receipt = f'original_run={run_id} current_run={current} source_sha={sha}'
        print(receipt, flush=True)
        with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as target:
            target.write(receipt + '\n')
        result = subprocess.run([str(Path(os.environ['RUNNER_TEMP']) / 'stripe-live.test'),
                                 '-test.run=^TestStripeLiveOwnerGate$', '-test.timeout=2m'],
                                cwd=Path(__file__).resolve().parents[1] / 'internal/db',
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                text=True, timeout=130, check=False)
        values = [json.loads(line.removeprefix('OSC_LIVE_SUMMARY '))
                  for line in result.stdout.splitlines() if line.startswith('OSC_LIVE_SUMMARY ')]
        if len(values) != 1:
            raise ValueError('missing projection')
        summary = public_summary(values[0])
        print(summary, flush=True)
        with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as target:
            target.write(f'{summary}\n\noriginal_run={run_id} current_run={current} source_sha={sha}\n')
        if result.returncode or values[0]['status'] != 'CANCELED':
            raise SystemExit('PENDING: smoke failed; owner inspection required; private diagnostics discarded')
    except Exception:
        # Never interpolate an exception or captured output into a public sink.
        raise SystemExit('PENDING: smoke unresolved; owner inspection required; private diagnostics discarded') from None


if __name__ == '__main__':
    main()
