/** Responsibility: exercise the mounted signed webhook over the disposable real API.
 * Dependencies: C19 ready-order SQL seed and the harness's private Fake signing key.
 * Caller: CI desktop/mobile acceptance. No route interception or purchase UI claim.
 */
import { createHmac } from 'node:crypto';
import { expect } from '@playwright/test';
import { test } from './fixture';
import { fixture } from './fixture';

// Credentials/capabilities must never enter automatic capture artifacts.
test.use({ trace: 'off', video: 'off', screenshot: 'off' });

test('real signed payment webhook rejects forgery and deduplicates concurrent facts', async ({ request }, testInfo) => {
  const index = testInfo.project.name === 'desktop' ? 1 : testInfo.project.name === 'mobile' ? 2 : 0;
  if (!index) throw new Error('Unknown payment fixture project');
  const encoded = process.env.OPENSITE_FAKE_WEBHOOK_KEY;
  if (!encoded || !/^[A-Za-z0-9_-]+$/.test(encoded)) throw new Error('Private CI Fake key is missing');
  const key = Buffer.from(encoded, 'base64url');
  if (key.length < 32 || key.toString('base64url') !== encoded) throw new Error('Invalid private CI Fake key');
  const event = {
    event_id: `c19-e2e-${testInfo.project.name}-captured`,
    account_id: `c19-e2e-${testInfo.project.name}`,
    order_id: `c1900003-0000-4000-8000-${String(index).padStart(12, '0')}`,
    order_version: 1,
    provider_ref: `fake_c19_e2e_${index}`,
    status: 'captured', amount_minor: 270, currency: 'USD',
  };
  const raw = JSON.stringify(event);
  const timestamp = String(Math.floor(Date.now() / 1000));
  const signature = `t=${timestamp},v1=${createHmac('sha256', key).update(`${timestamp}.${raw}`).digest('hex')}`;
  const endpoint = `${fixture.api}/api/v1/testing/payments/fake/webhook`;
  const post = (value: string) => request.post(endpoint, {
    data: raw, headers: { 'Content-Type': 'application/json', 'X-Fake-Signature': value },
    maxRedirects: 0, timeout: 15_000,
  });
  const forged = await post(`t=${timestamp},v1=${'0'.repeat(64)}`);
  expect(forged.status()).toBe(401);
  const responses = await Promise.all(Array.from({ length: 8 }, () => post(signature)));
  for (const response of responses) {
    expect(response.status()).toBe(200);
    expect(await response.json()).toEqual({ event_id: event.event_id, status: 'processed' });
  }
  const replay = await post(signature);
  expect(replay.status()).toBe(200);
  expect(await replay.json()).toEqual({ event_id: event.event_id, status: 'processed' });
  // run.py independently checks persisted inbox/hash/order/stock/journal facts.
});
