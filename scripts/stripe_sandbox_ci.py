#!/usr/bin/env python3
"""Responsibility: own one bounded CI-only Stripe listener and lifecycle process.
Dependencies: checksum-pinned Stripe CLI, test-key env, disposable migrated PG.
Caller: stripe-sandbox.yml. Never retain CLI output, credentials or raw events.
"""
import json
import os
import queue
import re
import subprocess
import threading
import time
import urllib.request


def main():
    env = dict(os.environ)
    key = env.get('STRIPE_TEST_SECRET_KEY', '')
    if not key.startswith('sk_test_') or len(key) <= 8 or any(c.isspace() for c in key):
        raise SystemExit('FAIL: test-mode secret required')
    print('::add-mask::' + key, flush=True)
    # Discover this test key's account without transmitting any customer fields.
    request = urllib.request.Request('https://api.stripe.com/v1/account',
                                     headers={'Authorization': 'Bearer ' + key})
    try:
        with urllib.request.urlopen(request, timeout=15) as response:
            account = json.load(response).get('id', '')
    except Exception:
        raise SystemExit('FAIL: test account lookup unavailable') from None
    if not re.fullmatch(r'acct_[A-Za-z0-9]+', account):
        raise SystemExit('FAIL: invalid test account identity')
    env['STRIPE_TEST_ACCOUNT_ID'] = account
    listener = subprocess.Popen([
        'stripe', '--api-key', key, 'listen', '--forward-to',
        'http://127.0.0.1:8787/api/v1/payments/stripe/webhook', '--events',
        'payment_intent.succeeded,payment_intent.payment_failed,payment_intent.canceled,refund.created',
    ], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, bufsize=1)
    secrets = queue.Queue(maxsize=1)

    def consume():
        for line in listener.stdout:
            match = re.search(r'whsec_[A-Za-z0-9]+', line)
            if match:
                secret = match[0]
                print('::add-mask::' + secret, flush=True)
                if secrets.empty():
                    secrets.put_nowait(secret)
        # All other CLI output is discarded: never upload a listener log.

    reader = threading.Thread(target=consume, daemon=True)
    reader.start()
    try:
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            if listener.poll() is not None:
                raise SystemExit('FAIL: Stripe listener exited before readiness')
            try:
                env['STRIPE_TEST_WEBHOOK_SECRET'] = secrets.get(timeout=1)
                break
            except queue.Empty:
                pass
        else:
            raise SystemExit('FAIL: Stripe listener readiness timeout')
        guard = subprocess.run([
            'go', 'test', '-race', '-count=1', '-run', '^TestStripe',
            './internal/modules/payments',
        ], env=env, timeout=120, check=False)
        if guard.returncode:
            raise SystemExit('FAIL: sandbox transport/signature safety gate')
        result = subprocess.run([
            'go', 'test', '-race', '-count=1', '-tags', 'pgx,stripe_sandbox',
            '-timeout=8m', '-v', '-run', '^TestStripeSandboxLifecycle$', './internal/db',
        ], env=env, timeout=540, check=False)
        if listener.poll() is not None:
            raise SystemExit('FAIL: Stripe listener exited during lifecycle')
        if result.returncode:
            raise SystemExit('FAIL: sandbox lifecycle gate')
        print('PASS: sandbox lifecycle; source ' + env.get('GITHUB_SHA', 'UNKNOWN'), flush=True)
    except subprocess.TimeoutExpired:
        raise SystemExit('FAIL: sandbox lifecycle timeout') from None
    finally:
        listener.terminate()
        try:
            listener.wait(timeout=10)
        except subprocess.TimeoutExpired:
            listener.kill()
            listener.wait(timeout=5)
        reader.join(timeout=5)


if __name__ == '__main__':
    main()
