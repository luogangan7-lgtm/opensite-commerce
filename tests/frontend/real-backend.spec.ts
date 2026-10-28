/** Real Go/PG visual lane: owned database fixtures and shipped Astro/React only.
 * Depends on start-real-backend.py and a transparent forwarding fault proxy.
 * No route interception; signed test cookies identify real seeded cart ownership.
 */
import { createHash, createHmac, randomBytes } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { test, expect, type Page } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { REAL_VISUAL_STATES } from '../../scripts/frontend/real-visual-manifest';

const site = 'https://127.0.0.1:4349';
const tenant = 'c0900001-0000-4000-8000-000000000001';
const sku = 'c2400008-0000-4000-8000-000000000001';
const backend = `/api/v1/t/${tenant}/checkout`;
test.use({ trace: 'off', video: 'off', screenshot: 'off', ignoreHTTPSErrors: true });

function runtime() {
  return JSON.parse(readFileSync('.work/visual-real/runtime.json', 'utf8')) as {
    pg: Record<string, string>; cookieKey: string; control: string;
  };
}
function sql(statement: string) {
  return execFileSync('psql', ['-X', '-qAt', '-v', 'ON_ERROR_STOP=1'], {
    input: statement, env: { ...process.env, ...runtime().pg }, encoding: 'utf8', timeout: 30_000,
  }).trim();
}
async function control(page: Page, action: string, data?: unknown) {
  const response = await page.request.fetch(`http://127.0.0.1:4181/__visual/${action}`, {
    method: action === 'status' ? 'GET' : 'POST', headers: { Authorization: `Bearer ${runtime().control}` },
    ...(data ? { data } : {}),
  });
  expect(response.ok()).toBe(true);
  return response;
}
async function fault(page: Page, path: string, method: string, mode: string) {
  await control(page, 'arm', { path, method, mode });
}
async function committedAttempt(page: Page, requestKey: string, before: string) {
  const receipt = await (await control(page, 'status')).json();
  if (receipt.status !== 201) throw new Error(`Discarded upstream was not committed (status ${receipt.status})`);
  expect(receipt).toMatchObject({ matched: true, status: 201 });
  expect(Number(sql(`SELECT count(*) FROM app.checkout_sessions WHERE tenant_id='${tenant}';`))).toBe(Number(before) + 1);
  const committed = sql(`SELECT id FROM app.checkout_sessions WHERE tenant_id='${tenant}' AND request_key='${requestKey}';`);
  expect(committed).toMatch(/^[0-9a-f-]{36}$/);
  return committed;
}
async function snapshot(page: Page, state: typeof REAL_VISUAL_STATES[213][number], pending = false) {
  await page.evaluate(async () => { await document.fonts.ready; });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  if (!pending) {
    const report = await new AxeBuilder({ page }).analyze();
    expect(report.violations.filter(item => ['serious', 'critical'].includes(item.impact ?? ''))).toEqual([]);
  }
  await expect(page).toHaveScreenshot(`real-213-${state}.png`, { fullPage: true });
}
async function review(page: Page) {
  await page.goto(`${site}/products/canvas-tote`);
  await page.getByRole('button', { name: 'Add to cart', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('Item added to cart.');
  await page.goto(`${site}/checkout`);
  for (const [label, value] of [['Email', 'visual-buyer@example.test'], ['Country code', 'US'],
    ['Address line 1', '1 Test Way'], ['City', 'Test City'], ['Postal code', '10001']]) {
    await page.getByLabel(label!, { exact: true }).fill(value!);
  }
  await page.getByRole('button', { name: 'Review totals', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Start test checkout', exact: true })).toBeEnabled();
}
async function start(page: Page) {
  await page.getByRole('button', { name: 'Start test checkout', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Fake payment: capture', exact: true })).toBeEnabled();
}

// Existing E2E seed pattern: create a sealed order/reservation and durable checkout,
// retaining every DB constraint. Payment finalization still uses the real signed HTTP path.
async function seededCompletion(page: Page, project: string, long = false) {
  const n = (project === 'desktop' ? 1 : 2) + (long ? 2 : 0);
  const id = (part: number) => `c449000${part}-0000-4000-8000-${String(n).padStart(12, '0')}`;
  const token = randomBytes(32).toString('base64url');
  const hash = createHash('sha256').update(token).digest('hex');
  const amount = long ? '4000000000000000000' : '3140';
  sql(`BEGIN; SELECT set_config('app.tenant_id','${tenant}',true);
    INSERT INTO app.carts(tenant_id,id,guest_token_hash,currency) VALUES('${tenant}','${id(1)}',decode('${hash}','hex'),'USD');
    DO $seed$ DECLARE r app.inventory_reservations; BEGIN
      r:=app.inventory_reserve('${sku}',1,'order','${id(2)}','visual-${n}',3600);
      PERFORM app.orders_create('${id(2)}','visual-${n}',sha256('visual-${n}'::bytea),NULL,
        '{"email":"visual-buyer@example.test"}','USD',0,'{}',
        jsonb_build_array(jsonb_build_object('id','${id(3)}','sku_id','${sku}','sku','C24-CANVAS-TOTE',
          'quantity',1,'unit_price_minor',${amount}::bigint,'line_net_minor',${amount}::bigint,'line_tax_minor',0,
          'line_total_minor',${amount}::bigint,'reservation_id',r.id,'tax_snapshot','{"category":"standard"}'::jsonb)),'[]');
    END $seed$;
    INSERT INTO app.checkout_sessions(tenant_id,id,actor_kind,actor_id,request_key,request_hash,cart_id,cart_version,
      order_id,address_snapshot,currency,amount_minor,items_net_minor,tax_minor,shipping_minor,payable_version)
      VALUES('${tenant}','${id(4)}','guest','${hash}','visual-${n}',sha256('visual-${n}'::bytea),'${id(1)}',1,
        '${id(2)}','{}','USD',${amount},${amount},0,0,1);
    UPDATE app.checkout_sessions SET phase='ready',provider_ref='visual_${n}',
      start_result=jsonb_build_object('checkout_id',id,'order_id',order_id,'state','ready','amount_minor',amount_minor,
        'currency',currency,'order_version',payable_version,'payment',jsonb_build_object('provider_ref','visual_${n}','status','authorized'))
      WHERE tenant_id='${tenant}' AND id='${id(4)}'; COMMIT;`);
  const session = Buffer.from(JSON.stringify({ s: randomBytes(32).toString('base64url'), c: id(1), t: token,
    tenant, checkout: id(4), x: Date.now() + 3_500_000 })).toString('base64url');
  const signature = createHmac('sha256', Buffer.from(runtime().cookieKey, 'base64url')).update(session).digest('base64url');
  await page.context().addCookies([{ name: '__Host-osc_storefront_cart', value: `${session}.${signature}`,
    url: site, secure: true, httpOnly: true, sameSite: 'Lax' }]);
  const cookie = (await page.context().cookies(site)).find(item => item.name === '__Host-osc_storefront_cart');
  expect(cookie).toMatchObject({ secure: true, httpOnly: true, sameSite: 'Lax', path: '/', domain: '127.0.0.1' });
  await page.goto(`${site}/order-complete`);
  await expect(page.locator('[data-completion-page]')).toHaveAttribute('aria-busy', 'false');
  return { checkout: id(4), order: id(2), amount };
}
async function capture(page: Page, checkout: { checkout: string; amount: string }) {
  const context = await (await page.request.get(`${site}/purchase/api/context`)).json();
  const response = await page.request.post(`${site}/purchase/api/payment`, {
    headers: { Origin: site, 'X-CSRF-Token': context.csrf },
    data: { checkout_id: checkout.checkout, outcome: 'captured', accepted_total: { amount_minor: checkout.amount, currency: 'USD' } },
  });
  expect(response.status()).toBe(200);
  expect(await response.json()).toMatchObject({ state: 'paid', payment: { status: 'captured' } });
}

test.describe('VISUAL-REAL #213', () => {
  test.afterEach(async ({ page }) => { await control(page, 'release'); });

  test('launcher removes its bootstrap role after failure, timeout and interruption, then provisions again', () => {
    // The launcher control flow and role DDL are real; business commands are isolated.
    execFileSync('python3', ['scripts/frontend/test_real_launcher.py'], { stdio: 'pipe', timeout: 120_000 });
  });

  test('committed replay guard rejects a real noncommitted first attempt before retry', async ({ page }) => {
    await review(page);
    const before = sql(`SELECT count(*) FROM app.checkout_sessions WHERE tenant_id='${tenant}';`);
    sql(`UPDATE app.price_entries SET unit_amount=4800 WHERE tenant_id='${tenant}' AND sku_id='${sku}';`);
    try {
      // Hold a real Go rejection: it has no checkout to replay. Never fabricate a DTO.
      await fault(page, backend, 'POST', 'hold');
      const request = page.waitForRequest(request => new URL(request.url()).pathname === '/purchase/api/checkout' && request.method() === 'POST');
      await page.getByRole('button', { name: 'Start test checkout', exact: true }).click();
      const key = await (await request).headerValue('Idempotency-Key');
      if (!key || !/^[0-9a-f-]{36}$/i.test(key)) throw new Error('Expected a UUID checkout request key');
      await expect.poll(async () => (await (await control(page, 'status')).json()).status).toBe(409);
      expect(sql(`SELECT count(*) FROM app.checkout_sessions WHERE tenant_id='${tenant}';`)).toBe(before);
      expect(sql(`SELECT count(*) FROM app.checkout_sessions WHERE tenant_id='${tenant}' AND request_key='${key}';`)).toBe('0');
      await expect(committedAttempt(page, key, before)).rejects.toThrow('Discarded upstream was not committed (status 409)');
      await control(page, 'release');
      await expect(page.getByRole('button', { name: 'Confirm updated total', exact: true })).toBeVisible();
    } finally {
      sql(`UPDATE app.price_entries SET unit_amount=2400 WHERE tenant_id='${tenant}' AND sku_id='${sku}';`);
    }
  });

  test('real checkout normal, request in flight, ambiguous failure and same-key retry', async ({ page }) => {
    await review(page);
    await snapshot(page, 'checkout-normal');
    await fault(page, backend, 'POST', 'hold');
    await page.getByRole('button', { name: 'Start test checkout', exact: true }).click();
    await expect.poll(async () => (await (await control(page, 'status')).json()).status).toBe(201);
    await expect(page.locator('[data-checkout-ready]')).toHaveAttribute('aria-busy', 'true');
    await snapshot(page, 'checkout-in-flight', true);
    await control(page, 'release');
    await expect(page.getByRole('button', { name: 'Fake payment: capture', exact: true })).toBeEnabled();
    // A separate cart loses the successful server response before BFF custody.
    await page.context().clearCookies();
    await review(page);
    const before = sql(`SELECT count(*) FROM app.checkout_sessions WHERE tenant_id='${tenant}';`);
    await fault(page, backend, 'POST', 'drop');
    const firstRequest = page.waitForRequest(request => new URL(request.url()).pathname === '/purchase/api/checkout' && request.method() === 'POST');
    await page.getByRole('button', { name: 'Start test checkout', exact: true }).click();
    const requestKey = await (await firstRequest).headerValue('Idempotency-Key');
    if (!requestKey || !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(requestKey)) throw new Error('Expected a UUID checkout request key');
    await expect(page.getByRole('alert')).toBeVisible();
    const committed = await committedAttempt(page, requestKey, before);
    await snapshot(page, 'checkout-ambiguous-failure');
    const replayResponse = page.waitForResponse(response => new URL(response.url()).pathname === '/purchase/api/checkout' && response.request().method() === 'POST');
    await start(page);
    const replay = await replayResponse;
    expect(replay.status()).toBe(201);
    expect(await replay.request().headerValue('Idempotency-Key')).toBe(requestKey);
    expect(await replay.json()).toMatchObject({ checkout_id: committed });
    expect(Number(sql(`SELECT count(*) FROM app.checkout_sessions WHERE tenant_id='${tenant}';`))).toBe(Number(before) + 1);
    await snapshot(page, 'checkout-retry');
  });

  test('real changed total requires acceptance before payment', async ({ page }) => {
    await review(page);
    sql(`UPDATE app.price_entries SET unit_amount=4800 WHERE tenant_id='${tenant}' AND sku_id='${sku}';`);
    try {
      await page.getByRole('button', { name: 'Start test checkout', exact: true }).click();
      await expect(page.getByRole('button', { name: 'Confirm updated total', exact: true })).toBeVisible();
      await expect(page.getByRole('button', { name: 'Start test checkout', exact: true })).toBeDisabled();
      await snapshot(page, 'checkout-changed-total');
      await page.getByRole('button', { name: 'Confirm updated total', exact: true }).click();
      await start(page);
      await snapshot(page, 'checkout-confirmed');
    } finally { sql(`UPDATE app.price_entries SET unit_amount=2400 WHERE tenant_id='${tenant}' AND sku_id='${sku}';`); }
  });

  for (const outcome of ['failed', 'canceled'] as const) test(`real ${outcome} payment`, async ({ page }) => {
    await review(page); await start(page);
    await page.getByRole('button', { name: outcome === 'failed' ? 'Fake payment: fail' : 'Fake payment: cancel', exact: true }).click();
    await expect(page.locator('[data-payment-status]')).toHaveAttribute('data-payment-status', outcome);
    await expect(page.getByRole('link', { name: 'View order confirmation', exact: true })).toHaveCount(0);
    await snapshot(page, `checkout-${outcome}`);
  });

  test('real completion empty, pending, loading, error, retry and persisted capture', async ({ page }, info) => {
    await page.goto(`${site}/order-complete`);
    await expect(page.locator('[data-completion-page]')).toHaveAttribute('aria-busy', 'false');
    await snapshot(page, 'completion-empty');
    const row = await seededCompletion(page, info.project.name);
    await expect(page.locator('[data-completion-ready]')).toHaveCount(0);
    await snapshot(page, 'completion-pending');
    await capture(page, row);
    await fault(page, `${backend}/${row.checkout}`, 'GET', 'hold');
    await page.goto(`${site}/order-complete`);
    await expect.poll(async () => (await (await control(page, 'status')).json()).status).toBe(200);
    await snapshot(page, 'completion-loading', true);
    await control(page, 'release');
    await expect(page.locator('[data-completion-ready]')).toBeVisible();
    await snapshot(page, 'completion-paid');
    // UUID is the shipped maximum identifier; no fictitious overlong DTO.
    await expect(page.getByRole('main')).toContainText(row.order);
    await snapshot(page, 'completion-long-order');
    await fault(page, `${backend}/${row.checkout}`, 'GET', '503');
    await page.reload();
    await expect(page.getByRole('alert')).toBeVisible();
    await snapshot(page, 'completion-error');
    await page.getByRole('button', { name: 'Try again', exact: true }).click();
    await expect(page.locator('[data-completion-ready]')).toBeVisible();
  });

  test('real completion retains a long exact bigint amount', async ({ page }, info) => {
    const row = await seededCompletion(page, info.project.name, true);
    await capture(page, row);
    await page.reload();
    await expect(page.locator('[data-completion-ready]')).toContainText('$40,000,000,000,000,000.00');
    await snapshot(page, 'completion-long-amount');
  });
});
