/** Real Go/PG visual lane: owned database fixtures and shipped Astro/React only.
 * Depends on start-real-backend.py and a transparent forwarding fault proxy.
 * No route interception; signed test cookies identify real seeded cart ownership.
 */
import { createHash, createHmac, randomBytes, randomUUID } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { test, expect, type Page, type APIResponse, type Locator } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { REAL_VISUAL_STATES } from '../../scripts/frontend/real-visual-manifest';

const site = 'https://127.0.0.1:4349';
const tenant = 'c0900001-0000-4000-8000-000000000001';
const sku = 'c2400008-0000-4000-8000-000000000001';
const backend = `/api/v1/t/${tenant}/checkout`;
test.use({ trace: 'off', video: 'off', screenshot: 'off', ignoreHTTPSErrors: true });

function runtime() {
  return JSON.parse(readFileSync('.work/visual-real/runtime.json', 'utf8')) as {
    pg: Record<string, string>; cookieKey: string; control: string; staffPassword: string; fakeWebhookKey: string;
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
      r:=app.inventory_reserve('${sku}',1,'order','${id(2)}','${id(5)}',3600);
      PERFORM app.orders_create('${id(2)}','${id(6)}',sha256('visual-${n}'::bytea),NULL,
        '{"email":"visual-buyer@example.test"}','USD',0,'{}',
        jsonb_build_array(jsonb_build_object('id','${id(3)}','sku_id','${sku}','sku','C24-CANVAS-TOTE',
          'quantity',1,'unit_price_minor',${amount}::bigint,'line_net_minor',${amount}::bigint,'line_tax_minor',0,
          'line_total_minor',${amount}::bigint,'reservation_id',r.id,'tax_snapshot','{"category":"standard"}'::jsonb)),'[]');
    END $seed$;
    INSERT INTO app.checkout_sessions(tenant_id,id,actor_kind,actor_id,request_key,request_hash,cart_id,cart_version,
      order_id,address_snapshot,currency,amount_minor,items_net_minor,tax_minor,shipping_minor,payable_version)
      VALUES('${tenant}','${id(4)}','guest','${hash}','${id(7)}',sha256('visual-${n}'::bytea),'${id(1)}',1,
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
    console.log(execFileSync('python3', ['scripts/frontend/test_real_launcher.py'], { stdio: 'pipe', timeout: 120_000 }).toString());
  });

  test('committed replay guard rejects a real noncommitted first attempt before retry', async ({ page }) => {
    await review(page);
    const before = sql(`SELECT count(*) FROM app.checkout_sessions WHERE tenant_id='${tenant}';`);
    sql(`UPDATE app.price_entries SET unit_amount=4800 WHERE tenant_id='${tenant}' AND sku_id='${sku}';`);
    try {
      // Discard a real Go rejection: it has no checkout to replay. Never fabricate a DTO.
      await fault(page, backend, 'POST', 'drop');
      const request = page.waitForRequest(request => new URL(request.url()).pathname === '/purchase/api/checkout' && request.method() === 'POST');
      await page.getByRole('button', { name: 'Start test checkout', exact: true }).click();
      const key = await (await request).headerValue('Idempotency-Key');
      if (!key || !/^[0-9a-f-]{36}$/i.test(key)) throw new Error('Expected a UUID checkout request key');
      await expect(page.getByRole('alert')).toBeVisible();
      await expect.poll(async () => (await (await control(page, 'status')).json()).status).toBe(409);
      expect(sql(`SELECT count(*) FROM app.checkout_sessions WHERE tenant_id='${tenant}';`)).toBe(before);
      expect(sql(`SELECT count(*) FROM app.checkout_sessions WHERE tenant_id='${tenant}' AND request_key='${key}';`)).toBe('0');
      await expect(committedAttempt(page, key, before)).rejects.toThrow('Discarded upstream was not committed (status 409)');
      console.log('VISUAL noncommitted first attempt: upstream 409, zero rows, committed-replay guard rejected');
      // After-only row counting would accept this later success. It is not a replay
      // of the first attempt, which the guard has already rejected above.
      sql(`UPDATE app.price_entries SET unit_amount=2400 WHERE tenant_id='${tenant}' AND sku_id='${sku}';`);
      const retryResponse = page.waitForResponse(response => new URL(response.url()).pathname === '/purchase/api/checkout' && response.request().method() === 'POST');
      await start(page);
      const retry = await retryResponse;
      expect(retry.status()).toBe(201);
      expect(await retry.request().headerValue('Idempotency-Key')).toBe(key);
      const created = await retry.json();
      expect(sql(`SELECT id FROM app.checkout_sessions WHERE tenant_id='${tenant}' AND request_key='${key}';`)).toBe(created.checkout_id);
      expect(Number(sql(`SELECT count(*) FROM app.checkout_sessions WHERE tenant_id='${tenant}';`))).toBe(Number(before) + 1);
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
    console.log('VISUAL committed replay: first upstream 201, row persisted before retry, same identity/key, exactly one row');
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

// #235 captures only after fragment custody and password removal. Automatic
// traces/video/failure captures remain disabled for this credential-bearing lane.
async function accountSnapshot(page: Page, state: typeof REAL_VISUAL_STATES[235][number], pending = false) {
  await page.evaluate(async () => { await document.fonts.ready; });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  expect(await page.locator('input[type="password"]').evaluateAll(inputs =>
    inputs.every(input => (input as HTMLInputElement).value === ''))).toBe(true);
  if (!pending) {
    const report = await new AxeBuilder({ page }).analyze();
    expect(report.violations.filter(item => ['serious', 'critical'].includes(item.impact ?? ''))).toEqual([]);
  }
  const busy = page.locator('[data-customer-ready], [data-customer-verification]');
  if (pending) await expect(busy).toHaveAttribute('aria-busy', 'true');
  await expect(page).toHaveScreenshot(`real-235-${state}.png`, { fullPage: true });
  if (pending) await expect(busy).toHaveAttribute('aria-busy', 'true');
}
const customerBackend = `/api/v1/t/${tenant}/customer`;
const historyBackend = `${customerBackend}/orders`;
const accountPassword = () => randomBytes(24).toString('base64url');
async function credentials(page: Page, email: string, password: string) {
  await page.getByLabel('Email', { exact: true }).fill(email);
  // Keep credential values out of Playwright step titles.
  await page.locator('input[type="password"]').evaluate((input, value) => {
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
    setter.call(input, value); input.dispatchEvent(new Event('input', { bubbles: true }));
  }, password);
}
async function mailboxProof(page: Page, email: string) {
  const response = await page.request.post('http://127.0.0.1:4182/proof', {
    headers: { Authorization: `Bearer ${runtime().control}` }, data: { email },
  });
  if (response.status() !== 200) throw new Error('Real SMTP notification proof was not delivered');
  const payload = await response.json();
  if (typeof payload.token !== 'string' || !/^[A-Za-z0-9_-]{43}$/.test(payload.token)) throw new Error('Invalid SMTP proof');
  return payload.token as string;
}
async function openProof(page: Page, proof: string) {
  await page.goto(`${site}/account`);
  // Browser-only navigation: fragment never reaches an HTTP server or step URL.
  await page.evaluate(value => location.assign(`/customer/verify#token=${value}`), proof);
  await expect(page.getByRole('button', { name: 'Verify email', exact: true })).toBeEnabled();
  await expect(page).toHaveURL(`${site}/customer/verify`);
  expect(await page.evaluate(value => document.documentElement.outerHTML.includes(value), proof)).toBe(false);
}
async function registerAccount(page: Page, email: string, password: string) {
  await page.goto(`${site}/account`); await credentials(page, email, password);
  await page.getByRole('button', { name: 'Register', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('verification email has been sent');
  await expect(page.getByRole('link', { name: 'Order history', exact: true })).toHaveCount(0);
  return mailboxProof(page, email);
}

// #250 follows scripts/e2e/b2b.py's identity seed, then real Go password login.
// #235 owns registration/proof coverage. Sharing its IP for 34 writes/minute
// exhausts the real 30/min budget; seed identities, never relax/reset that budget.
async function historyCustomer(page: Page, project: string, suffix: string) {
  if (!['desktop', 'mobile'].includes(project) || !/^[a-z0-9-]+$/.test(suffix)) throw new Error('Invalid owned customer fixture');
  const email = `visual-250-${project}-${suffix}@example.test`, password = runtime().staffPassword, id = randomUUID();
  sql(`BEGIN; INSERT INTO app.customers(tenant_id,id,email,verified_at)
    VALUES('${tenant}','${id}','${email}',clock_timestamp());
    INSERT INTO app.customer_credentials(tenant_id,customer_id,password_hash)
      SELECT '${tenant}','${id}',password_hash FROM app.users WHERE email='visual-owner@example.test'; COMMIT;`);
  await historyLogin(page, email, password);
  return { id, email, password };
}
async function historyLogin(page: Page, email: string, password: string) {
  await page.goto(`${site}/account`); await credentials(page, email, password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('link', { name: 'Order history', exact: true })).toBeVisible();
}
const historyPanel = (page: Page) => page.locator('.purchase-page[aria-label="Order history"]');
type Box = { x: number; y: number; width: number; height: number };
function assertDateMaskBounds(mask: Box, date: Box, protectedBoxes: Box[]) {
  const right = (box: Box) => box.x + box.width, bottom = (box: Box) => box.y + box.height;
  if (mask.x < date.x || mask.y < date.y || right(mask) > right(date) || bottom(mask) > bottom(date))
    throw new Error('Date mask extends outside time element');
  if (protectedBoxes.some(box => mask.x < right(box) && right(mask) > box.x && mask.y < bottom(box) && bottom(mask) > box.y))
    throw new Error('Date mask overlaps status or amount');
}
// Only the runtime B2B order may supply an exact date mask. Historical dates
// remain visible; real API timestamp/display assertions precede every mask.
async function historySnapshot(page: Page, state: typeof REAL_VISUAL_STATES[250][number], pending = false, fullPage = true, mask: Locator[] = [], maskColor?: string) {
  await page.evaluate(async () => { await document.fonts.ready; });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'history fits viewport').toBe(true);
  if (!pending) {
    const report = await new AxeBuilder({ page }).analyze();
    expect(report.violations.filter(item => ['serious', 'critical'].includes(item.impact ?? ''))).toEqual([]);
  }
  const panel = historyPanel(page);
  if (state !== 'account-entry') await expect(panel).toHaveAttribute('aria-busy', String(pending));
  await expect(page).toHaveScreenshot(`real-250-${state}.png`, { fullPage, mask, maskColor });
  if (state !== 'account-entry') await expect(panel).toHaveAttribute('aria-busy', String(pending));
}

// Historical display fixtures use fixed dates/numbers, not a browser-clock expiry.
// Use the existing Orders executor with every trigger/FK/RLS/check active. The
// large number is a legal historical snapshot; no trigger suspension or DTO mock.
function historyOrders(customer: { id: string; email: string }, project: string, batch: number, count = 1, long = false, guest = false, variant?: 'normal' | 'multiline') {
  const orderIds = Array.from({ length: count }, () => randomUUID());
  const number = variant ? (variant === 'normal' ? 701234n : 705678n) + BigInt(project === 'mobile' ? 100 : 0) : 9007199254741000n + BigInt(project === 'mobile' ? 1000 : 0) + BigInt(batch * 100);
  const code = long ? 'S'.repeat(255) : 'C24-CANVAS-TOTE';
  const stamp = variant === 'normal' ? '2026-01-02T11:00:00Z' : variant === 'multiline' ? '2026-01-02T10:00:00Z' : '2026-01-02T12:00:00Z';
  sql(`BEGIN; SELECT set_config('app.tenant_id','${tenant}',true);
    DO $seed$ DECLARE r app.inventory_reservations; r2 app.inventory_reservations; oid uuid; n integer; amount bigint; BEGIN
      FOR n IN 0..${count - 1} LOOP
        oid:=(ARRAY[${orderIds.map(id => `'${id}'::uuid`).join(',')}])[n+1]; amount:=CASE WHEN ${long} AND n=0 THEN 9223372036854775807 ELSE 3140 END;
        r:=app.inventory_reserve('${sku}',1,'order',oid,gen_random_uuid()::text,3600);
        IF ${variant === 'multiline'} THEN
          r2:=app.inventory_reserve('${sku}',1,'order',oid,gen_random_uuid()::text,3600);
        END IF;
        SET LOCAL ROLE app_orders_executor;
        INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,customer_id,guest_contact,
          currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor,created_at)
        VALUES('${tenant}',oid,${number}::bigint+n,gen_random_uuid()::text,sha256(oid::text::bytea),
          ${guest ? 'NULL' : `'${customer.id}'::uuid`},${guest ? `jsonb_build_object('email','${customer.email}')` : 'NULL'},
          'USD',amount,0,0,'{}',amount,'${stamp}'::timestamptz-n*interval '1 second');
        INSERT INTO app.order_lines(tenant_id,id,order_id,sku_id,sku,quantity,unit_price_minor,line_net_minor,
          line_tax_minor,line_total_minor,reservation_id,tax_snapshot)
        VALUES('${tenant}',CASE WHEN ${variant === 'multiline'} THEN 'c2500001-0000-4000-8000-00000000000${project === 'mobile' ? 2 : 1}'::uuid ELSE uuidv7() END,oid,'${sku}',CASE WHEN n=0 THEN '${code}' ELSE 'C24-CANVAS-TOTE' END,
          1,amount,amount,0,amount,r.id,'{}');
        IF ${variant === 'multiline'} THEN
          INSERT INTO app.order_lines(tenant_id,id,order_id,sku_id,sku,quantity,unit_price_minor,line_net_minor,
            line_tax_minor,line_total_minor,reservation_id,tax_snapshot)
          VALUES('${tenant}','c2500002-0000-4000-8000-00000000000${project === 'mobile' ? 2 : 1}',oid,'${sku}','SECOND-LINE',1,0,0,0,0,r2.id,'{}');
        END IF;
        UPDATE app.orders SET sealed=true WHERE tenant_id='${tenant}' AND id=oid;
        RESET ROLE;
      END LOOP;
    END $seed$; COMMIT;`);
  if (guest) sql(`BEGIN; SELECT set_config('app.tenant_id','${tenant}',true); SELECT app.customer_claim_guest_orders('${customer.id}'); COMMIT;`);
  const rows = JSON.parse(sql(`SELECT jsonb_agg(jsonb_build_object('id',id,'number',order_number::text)
    ORDER BY created_at DESC,id DESC) FROM app.orders WHERE tenant_id='${tenant}' AND id = ANY(ARRAY[${orderIds.map(id => `'${id}'::uuid`).join(',')}]);`)) as { id: string; number: string }[];
  expect(rows).toHaveLength(count);
  return { rows, code };
}
async function historyReady(page: Page) {
  await expect(historyPanel(page)).toHaveAttribute('aria-busy', 'false');
  await expect(historyPanel(page)).toBeFocused();
}
function responseCookies(response: APIResponse) {
  return response.headersArray().filter(header => header.name.toLowerCase() === 'set-cookie')
    .map(header => header.value.split(';')[0]).join('; ');
}
async function customerHeaders(page: Page, api: string) {
  const cookie = (await page.context().cookies(site)).find(item => item.name === '__Host-osc_customer_session');
  if (!cookie) throw new Error('Real customer session missing');
  const headers = { Host: '127.0.0.1', Origin: 'https://store.visual.test', Cookie: `${cookie.name}=${cookie.value}` };
  const response = await page.request.get(`${api}${customerBackend}/session`, { headers });
  expect(response.status()).toBe(200);
  return { ...headers, 'X-CSRF-Token': (await response.json()).csrf_token as string };
}

test.describe('VISUAL-REAL #250', () => {
  test.afterEach(async ({ page }) => { await control(page, 'release'); });

  test('real history entry, long snapshots, multiline detail and restored keyboard focus', async ({ page }, info) => {
    const customer = await historyCustomer(page, info.project.name, 'long');
    const normal = historyOrders(customer, info.project.name, 8, 1, false, false, 'normal').rows[0]!;
    await historySnapshot(page, 'account-entry');
    await page.getByRole('link', { name: 'Order history', exact: true }).click();
    await historyReady(page);
    const normalOrder = page.getByRole('button', { name: `Order #${normal.number}`, exact: true });
    await expect(normalOrder).toBeVisible();
    await historySnapshot(page, 'history-list');
    await normalOrder.click(); await historyReady(page);
    await expect(page.getByRole('region', { name: 'Order details', exact: true })).toContainText('C24-CANVAS-TOTE');
    await historySnapshot(page, 'history-detail');
    const fixture = historyOrders(customer, info.project.name, 1, 1, true), row = fixture.rows[0]!;
    await page.goto(`${site}/account/orders`); await historyReady(page);
    const order = page.getByRole('button', { name: `Order #${row.number}`, exact: true });
    await expect(order).toBeVisible();
    await expect(historyPanel(page)).toContainText('$92,233,720,368,547,758.07');
    await historySnapshot(page, 'history-long-order');
    await fault(page, `${historyBackend}/${row.id}`, 'GET', 'hold');
    await order.click();
    await expect.poll(async () => (await (await control(page, 'status')).json()).status).toBe(200);
    await expect(page.getByRole('status')).toHaveText('Loading orders…');
    await historySnapshot(page, 'history-detail-loading', true);
    await control(page, 'release'); await historyReady(page);
    const detail = page.getByRole('region', { name: 'Order details', exact: true });
    await expect(detail).toContainText(fixture.code);
    await expect(detail).toContainText('$92,233,720,368,547,758.07');
    const line = detail.locator('.cart-line');
    expect(await line.evaluate(element => element.scrollWidth <= element.clientWidth), 'long SKU fits its detail line').toBe(true);
    expect(await line.evaluate(element => {
      const range = document.createRange(); range.selectNodeContents(element.firstChild!);
      return range.getClientRects().length;
    }), 'long SKU wraps into multiple visible lines').toBeGreaterThan(1);
    await historySnapshot(page, 'history-long-sku');
    const multi = historyOrders(customer, info.project.name, 9, 1, false, false, 'multiline').rows[0]!;
    await page.goto(`${site}/account/orders`); await historyReady(page);
    await page.getByRole('button', { name: `Order #${multi.number}`, exact: true }).click(); await historyReady(page);
    await expect(detail.locator('.cart-line')).toHaveCount(2);
    await expect(detail).toContainText('C24-CANVAS-TOTE');
    await expect(detail).toContainText('SECOND-LINE');
    await historySnapshot(page, 'history-multiline');
    await page.getByRole('button', { name: 'Back to orders', exact: true }).click();
    await historyReady(page);
    expect(await historyPanel(page).evaluate(element => element.getBoundingClientRect().top >= 0)).toBe(true);
    await page.keyboard.press('Tab'); await expect(order).toBeFocused();
    expect(await order.evaluate(element => Number.parseFloat(getComputedStyle(element).outlineWidth))).toBeGreaterThan(0);
    await historySnapshot(page, 'history-focus-restored');
  });

  test('real cursor pagination, held loading, scroll focus and retry without disabled actions', async ({ page }, info) => {
    const customer = await historyCustomer(page, info.project.name, 'pages');
    const fixture = historyOrders(customer, info.project.name, 2, 26);
    const listed = page.waitForResponse(response => new URL(response.url()).pathname === '/purchase/api/history');
    await page.goto(`${site}/account/orders`); const first = await (await listed).json();
    await historyReady(page);
    expect(first.items).toHaveLength(25); expect(first.next_cursor).toMatch(/^[A-Za-z0-9_-]+$/);
    await expect(historyPanel(page).getByRole('button', { name: /^Order #/ })).toHaveCount(25);
    const next = page.getByRole('button', { name: 'Next orders', exact: true });
    await next.focus(); await page.keyboard.press('Shift+Tab'); await page.keyboard.press('Tab');
    await expect(next).toBeFocused();
    expect(await page.evaluate(() => scrollY)).toBeGreaterThan(0);
    await historySnapshot(page, 'history-focus-scroll', false, false);
    await fault(page, historyBackend, 'GET', 'hold');
    const continuation = page.waitForResponse(response => {
      const url = new URL(response.url());
      return url.pathname === '/purchase/api/history' && url.searchParams.get('cursor') === first.next_cursor;
    });
    await page.keyboard.press('Enter');
    await expect.poll(async () => (await (await control(page, 'status')).json()).status).toBe(200);
    // Read-only history has no disabled mutation action. Busy removes navigation.
    await expect(historyPanel(page).getByRole('button')).toHaveCount(0);
    await historySnapshot(page, 'history-pagination-loading', true);
    await control(page, 'release');
    expect((await continuation).status()).toBe(200); await historyReady(page);
    await expect(page.getByRole('button', { name: `Order #${fixture.rows[25]!.number}`, exact: true })).toBeVisible();
    await expect(next).toHaveCount(0);
    expect(await page.evaluate(() => scrollY)).toBe(0);
    await historySnapshot(page, 'history-pagination');
    await page.reload(); await historyReady(page);
    await fault(page, historyBackend, 'GET', '503'); await next.click();
    await expect(page.getByRole('alert')).toHaveText('Orders could not be loaded. Try again.');
    expect(await (await control(page, 'status')).json()).toMatchObject({ matched: true, status: 200 });
    await historySnapshot(page, 'history-pagination-503');
    await control(page, 'release'); await page.getByRole('button', { name: 'Try again', exact: true }).click();
    await historyReady(page); await expect(next).toBeEnabled();
  });

  test('real empty, held list, 503 and expired session remove private cached history', async ({ page }, info) => {
    const customer = await historyCustomer(page, info.project.name, 'errors');
    await fault(page, historyBackend, 'GET', 'hold'); await page.goto(`${site}/account/orders`);
    await expect.poll(async () => (await (await control(page, 'status')).json()).status).toBe(200);
    await expect(page.getByRole('status')).toHaveText('Loading orders…');
    await historySnapshot(page, 'history-loading', true);
    await control(page, 'release'); await historyReady(page);
    await expect(page.getByRole('main')).toContainText('No orders yet.');
    await expect(historyPanel(page).locator('button:disabled')).toHaveCount(0);
    await historySnapshot(page, 'history-empty');
    const fixture = historyOrders(customer, info.project.name, 3);
    await page.reload(); await historyReady(page);
    await expect(page.getByRole('button', { name: `Order #${fixture.rows[0]!.number}`, exact: true })).toBeVisible();
    await fault(page, historyBackend, 'GET', '503'); await page.reload();
    await expect(page.getByRole('alert')).toHaveText('Orders could not be loaded. Try again.');
    expect(await (await control(page, 'status')).json()).toMatchObject({ matched: true, status: 200 });
    await historySnapshot(page, 'history-list-503'); await control(page, 'release');
    await page.getByRole('button', { name: 'Try again', exact: true }).click(); await historyReady(page);
    sql(`WITH stamp AS MATERIALIZED (SELECT clock_timestamp() AS n)
      UPDATE app.customer_sessions SET created_at=stamp.n-interval '2 hours',
      last_seen_at=stamp.n-interval '1 hour',idle_expires_at=stamp.n-interval '1 second',
      expires_at=stamp.n-interval '1 second' FROM stamp
      WHERE tenant_id='${tenant}' AND customer_id='${customer.id}' AND revoked_at IS NULL;`);
    await page.reload(); await expect(page.getByRole('alert')).toHaveText('Sign in to view your orders.');
    await expect(historyPanel(page).getByRole('button', { name: /^Order #/ })).toHaveCount(0);
    await historySnapshot(page, 'history-expired-session');
    await historyLogin(page, customer.email, customer.password);
    await page.getByRole('link', { name: 'Order history', exact: true }).click(); await historyReady(page);
    await expect(page.getByRole('button', { name: `Order #${fixture.rows[0]!.number}`, exact: true })).toBeVisible();
  });

  test('real detail ownership 404 and forwarded 503 recover through a fresh list', async ({ page }, info) => {
    const customer = await historyCustomer(page, info.project.name, 'ownership');
    const fixture = historyOrders(customer, info.project.name, 4, 1, false, true), row = fixture.rows[0]!;
    await page.goto(`${site}/account/orders`); await historyReady(page);
    const order = page.getByRole('button', { name: `Order #${row.number}`, exact: true });
    await expect(order).toBeVisible();
    sql(`DELETE FROM app.customer_guest_order_links WHERE tenant_id='${tenant}' AND customer_id='${customer.id}' AND order_id='${row.id}';`);
    const denied = page.waitForResponse(response => new URL(response.url()).pathname === `/purchase/api/history/${row.id}`);
    await order.click(); expect((await denied).status()).toBe(404);
    await expect(page.getByRole('alert')).toHaveText('Order not found.');
    await expect(page.getByRole('region', { name: 'Order details', exact: true })).toHaveCount(0);
    await historySnapshot(page, 'history-not-found');
    sql(`BEGIN; SELECT set_config('app.tenant_id','${tenant}',true); SELECT app.customer_claim_guest_orders('${customer.id}'); COMMIT;`);
    await page.getByRole('button', { name: 'Try again', exact: true }).click(); await historyReady(page);
    await fault(page, `${historyBackend}/${row.id}`, 'GET', '503'); await order.click();
    await expect(page.getByRole('alert')).toHaveText('Orders could not be loaded. Try again.');
    expect(await (await control(page, 'status')).json()).toMatchObject({ matched: true, status: 200 });
    await historySnapshot(page, 'history-detail-503'); await control(page, 'release');
    await page.getByRole('button', { name: 'Try again', exact: true }).click(); await historyReady(page);
    await order.click(); await expect(page.getByRole('region', { name: 'Order details', exact: true })).toContainText('$31.40');
  });

  test('real B2B approved quote and captured 27 dollar order list and detail', async ({ page, browser, playwright }, info) => {
    const api = 'http://127.0.0.1:4184', base = `${api}/api/v1/t/${tenant}`;
    const buyer = await historyCustomer(page, info.project.name, 'b2b-buyer');
    const approver = await browser.newPage({ ignoreHTTPSErrors: true, viewport: page.viewportSize()! });
    const staff = await playwright.request.newContext();
    try {
      const other = await historyCustomer(approver, info.project.name, 'b2b-approver'), company = randomUUID();
      sql(`BEGIN; INSERT INTO app.buyer_companies(tenant_id,id,name) VALUES('${tenant}','${company}','Visual B2B');
        INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code)
        VALUES('${tenant}','${company}','${buyer.id}','purchaser'),('${tenant}','${company}','${other.id}','approver'); COMMIT;`);
      const buyerHeaders = await customerHeaders(page, api), approverHeaders = await customerHeaders(approver, api);
      const origin = 'https://admin.visual.test';
      const challenge = await staff.get(`${api}/api/v1/auth/login`, { headers: { Origin: origin } });
      expect(challenge.status()).toBe(200);
      const login = await staff.post(`${api}/api/v1/auth/login`, {
        headers: { Origin: origin, Cookie: responseCookies(challenge), 'X-CSRF-Token': (await challenge.json()).csrf_token },
        data: { email: 'visual-owner@example.test', password: runtime().staffPassword },
      });
      expect(login.status()).toBe(201);
      const staffHeaders = { Host: '127.0.0.1', Origin: origin, Cookie: responseCookies(login), 'X-CSRF-Token': (await login.json()).csrf_token as string };
      // A login is global; the fixture must also confer real tenant admission.
      expect((await staff.get(`${base}/orders?limit=1`, { headers: staffHeaders })).status()).toBe(200);
      const inquiry = await page.request.post(`${api}/api/public/v1/inquiries`, { headers: { Host: '127.0.0.1' },
        data: { buyer_email: buyer.email, buyer_name: 'Visual B2B', message: 'Negotiated mugs', quantity: 2,
          product_id: 'c0900007-0000-4000-8000-000000000002' } });
      expect(inquiry.status()).toBe(202);
      const quoteResponse = await staff.post(`${base}/quotes`, {
        headers: { ...staffHeaders, 'If-Match': '*', 'Idempotency-Key': randomUUID() }, data: {
          site_id: 'c0900003-0000-4000-8000-000000000001', inquiry_id: (await inquiry.json()).id,
          company_id: company, currency: 'USD', expires_at: sql(`SELECT to_char((clock_timestamp()+interval '1 day') AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"');`),
          lines: [{ product_id: 'c0900007-0000-4000-8000-000000000002', variant_id: 'c2400008-0000-4000-8000-000000000002',
            description: 'Negotiated ceramic mugs', quantity: 2, unit_price: 1000, tax_category: 'standard' }],
          destination: { country: 'US', subdivision: '' }, shipping_profile_id: 'c2400014-0000-4000-8000-000000000001', weight_g: 200,
        },
      });
      expect(quoteResponse.status()).toBe(201); const quote = await quoteResponse.json();
      expect(quote).toMatchObject({ status: 'draft', snapshot: { total: 2700 } });
      const seller = `${base}/quotes/${quote.id}`, publicQuote = `${api}/api/public/v1/quotes/${quote.id}`;
      const sent = await staff.post(`${seller}/send`, { data: {}, headers: { ...staffHeaders, 'If-Match': quoteResponse.headers().etag!, 'Idempotency-Key': randomUUID() } });
      expect(sent.status()).toBe(200);
      const accepted = await page.request.post(`${publicQuote}/accept`, { data: {}, headers: { ...buyerHeaders, 'If-Match': sent.headers().etag!, 'Idempotency-Key': randomUUID() } });
      expect(accepted.status()).toBe(200); expect((await accepted.json()).status).toBe('accepted_pending_approval');
      const approved = await approver.request.post(`${publicQuote}/approve`, { data: {}, headers: { ...approverHeaders, 'If-Match': accepted.headers().etag!, 'Idempotency-Key': randomUUID() } });
      expect(approved.status()).toBe(200); expect((await approved.json()).status).toBe('approved');
      const converted = await page.request.post(`${seller}/orders`, { data: {}, headers: { ...buyerHeaders, 'Idempotency-Key': randomUUID() } });
      expect(converted.status()).toBe(201); const receipt = await converted.json();
      expect(receipt).toMatchObject({ amount: '2700', currency: 'USD', state: 'pending_payment' });
      expect(receipt.order_id).toMatch(/^[0-9a-f-]{36}$/);
      const payable = await staff.get(`${base}/orders/${receipt.order_id}`, { headers: staffHeaders });
      expect(payable.status()).toBe(200);
      const event = { event_id: `visual-250:${info.project.name}:captured`, account_id: 'c09d-e2e', order_id: receipt.order_id,
        order_version: Number((await payable.json()).version), provider_ref: receipt.provider_ref, status: 'captured', amount_minor: 2700, currency: 'USD' };
      const raw = JSON.stringify(event), stamp = sql('SELECT floor(extract(epoch FROM clock_timestamp()))::bigint;');
      const signature = `t=${stamp},v1=${createHmac('sha256', Buffer.from(runtime().fakeWebhookKey, 'base64url')).update(`${stamp}.${raw}`).digest('hex')}`;
      const captured = await page.request.post(`${api}/api/v1/testing/payments/fake/webhook`, { data: raw, headers: { 'Content-Type': 'application/json', 'X-Fake-Signature': signature } });
      expect(captured.status()).toBe(200); expect(await captured.json()).toMatchObject({ status: 'processed' });
      expect(sql(`SELECT count(*) FROM app.quote_conversions c JOIN app.orders o ON (o.tenant_id,o.id)=(c.tenant_id,c.order_id)
        WHERE c.tenant_id='${tenant}' AND c.order_id='${receipt.order_id}' AND c.customer_id='${buyer.id}' AND o.state='paid' AND o.total_minor=2700;`)).toBe('1');
      const foreign = await approver.request.get(`${base}/customer/orders/${receipt.order_id}`, { headers: approverHeaders });
      expect(foreign.status()).toBe(404);
      await page.goto(`${site}/account/orders`); await historyReady(page);
      await expect(historyPanel(page)).toContainText('$27.00'); await expect(historyPanel(page)).toContainText('paid');
      const persisted = await page.request.get(`${base}/customer/orders/${receipt.order_id}`, { headers: buyerHeaders });
      expect(persisted.status()).toBe(200);
      const createdAt = (await persisted.json()).order.created_at as string;
      const order = page.getByRole('button', { name: `Order #${receipt.order_number}`, exact: true });
      const row = historyPanel(page).locator('li.cart-line').filter({ has: order });
      const date = row.locator('time');
      await expect(row.locator('.cart-line-heading p')).toHaveText('$27.00');
      await expect(date.locator('..')).toContainText('paid');
      await expect(date).toHaveCount(1);
      await expect(date).toHaveAttribute('datetime', createdAt);
      await expect(date).toHaveText(/^(?:[1-9]|1[0-2])\/(?:[1-9]|[12][0-9]|3[01])\/\d{4}$/);
      await expect(date).toHaveText(new Date(createdAt).toLocaleDateString('en-US', { timeZone: 'UTC' }));
      // Exercise a different calendar date without changing the real backend fact.
      await page.evaluate(async () => { await document.fonts.ready; });
      // Proportional date glyphs change the mask's width. Matching the actual
      // flat canvas keeps that rectangle's edge from becoming a date-shaped signal.
      const maskColor = await page.locator('body').evaluate(node => getComputedStyle(node).backgroundColor);
      const original = await date.innerText();
      const beforeBox = await date.boundingBox();
      const amountBox = await row.locator('.cart-line-heading p').boundingBox();
      const statusBox = await date.locator('..').evaluate(element => {
        const range = document.createRange(); range.selectNodeContents(element.firstChild!);
        const r = range.getBoundingClientRect(); return { x: r.x, y: r.y, width: r.width, height: r.height };
      });
      if (!beforeBox || !amountBox) throw new Error('Missing date mask geometry');
      const maskBox = async (locator: Locator) => {
        const box = await locator.boundingBox();
        if (!box) throw new Error('Missing date mask geometry');
        return box;
      };
      const dateMasks = [date];
      // The adversarial candidate spans the whole row; it must be rejected.
      const awaitedRowBox = await maskBox(row);
      expect(() => assertDateMaskBounds(awaitedRowBox, beforeBox, [statusBox, amountBox])).toThrow('Date mask extends outside time element');
      assertDateMaskBounds(await maskBox(dateMasks[0]), beforeBox, [statusBox, amountBox]);
      const before = await page.screenshot({ fullPage: true, mask: dateMasks, maskColor });
      await date.evaluate(node => { node.textContent = '12/31/2099'; });
      const afterBox = await date.boundingBox();
      if (!afterBox) throw new Error('Missing shifted date mask geometry');
      assertDateMaskBounds(await maskBox(dateMasks[0]), afterBox, [statusBox, amountBox]);
      const after = await page.screenshot({ fullPage: true, mask: dateMasks, maskColor });
      await date.evaluate((node, value) => { node.textContent = value; }, original);
      console.info('VISUAL runtime date mask geometry', { beforeBox, afterBox, maskColor });
      if (!after.equals(before)) {
        // This page contains only the owned synthetic order, never credentials.
        await info.attach('runtime-date-before', { body: before, contentType: 'image/png' });
        await info.attach('runtime-date-shifted', { body: after, contentType: 'image/png' });
      }
      expect(after.equals(before), 'date change must not alter screenshot pixels').toBe(true);
      assertDateMaskBounds(await maskBox(dateMasks[0]), await maskBox(date), [statusBox, amountBox]);
      await historySnapshot(page, 'b2b-paid-list', false, true, dateMasks, maskColor);
      await order.click(); await historyReady(page);
      await expect(page.getByRole('region', { name: 'Order details', exact: true })).toContainText('$27.00');
      await expect(page.getByRole('region', { name: 'Order details', exact: true })).toContainText('paid');
      // The shipped detail has no date element; its entire image stays compared.
      await expect(historyPanel(page).locator('time')).toHaveCount(0);
      await historySnapshot(page, 'b2b-paid-detail');
    } finally { await approver.close(); await staff.dispose(); }
  });
});

test.describe('VISUAL-REAL #235', () => {
  test('notification sink rejects missing authority and malformed reads', async ({ page }) => {
    expect((await page.request.post('http://127.0.0.1:4182/proof', { data: { email: 'visual@example.test' } })).status()).toBe(403);
    expect((await page.request.post('http://127.0.0.1:4182/proof', {
      headers: { Authorization: `Bearer ${runtime().control}` }, data: { email: 'external@example.com' },
    })).status()).toBe(400);
  });
  test.afterEach(async ({ page }) => { await control(page, 'release'); });

  test('real guest, validation, long email/help and keyboard focus', async ({ page }) => {
    await page.goto(`${site}/account`);
    await expect(page.getByText('You are browsing as a guest.', { exact: true })).toBeVisible();
    await accountSnapshot(page, 'account-guest');
    await page.getByLabel('Email', { exact: true }).click();
    await page.keyboard.press('Shift+Tab'); await page.keyboard.press('Tab');
    await expect(page.getByLabel('Email', { exact: true })).toBeFocused();
    await accountSnapshot(page, 'account-keyboard-focus');
    await page.getByRole('button', { name: 'Register', exact: true }).click();
    await expect(page.getByText('Enter a valid email address.', { exact: true })).toBeVisible();
    await expect(page.getByText('Enter your password. New passwords must contain 12–1024 UTF-8 bytes.', { exact: true })).toBeVisible();
    await accountSnapshot(page, 'account-validation-errors');
    const longEmail = `${'long-customer.'.repeat(4)}buyer@${'department.'.repeat(9)}example.test`;
    await page.getByLabel('Email', { exact: true }).fill(longEmail);
    await expect(page.getByLabel('Email', { exact: true })).toHaveValue(longEmail);
    await accountSnapshot(page, 'account-long-email');
    await expect(page.getByText('Registration requires 12–1024 UTF-8 bytes. Sign in with your existing password.', { exact: true })).toBeVisible();
    await accountSnapshot(page, 'account-long-help');
  });

  test('real registration, disabled in-flight fields, notice, verification and login', async ({ page }, info) => {
    const email = `visual-235-${info.project.name}@example.test`, password = accountPassword();
    await page.goto(`${site}/account`); await credentials(page, email, password);
    await fault(page, `${customerBackend}/registrations`, 'POST', 'hold');
    await page.getByRole('button', { name: 'Register', exact: true }).click();
    await expect.poll(async () => (await (await control(page, 'status')).json()).status).toBe(202);
    await expect(page.locator('[data-customer-ready]')).toHaveAttribute('aria-busy', 'true');
    await expect(page.getByLabel('Email', { exact: true })).toBeDisabled();
    await expect(page.getByRole('button', { name: 'Register', exact: true })).toBeDisabled();
    // Clear only the disabled input's presentation while the real request is held;
    // its body has already reached Go and no password is rendered in evidence.
    await page.locator('input[type="password"]').evaluate(input => { (input as HTMLInputElement).value = ''; });
    await accountSnapshot(page, 'account-in-flight', true);
    await control(page, 'release');
    await expect(page.getByRole('status')).toContainText('verification email has been sent');
    await accountSnapshot(page, 'account-registration-notice');
    const proof = await mailboxProof(page, email);
    const before = sql(`SELECT count(*) FROM app.customer_auth_tokens WHERE tenant_id='${tenant}' AND consumed_at IS NOT NULL;`);
    await openProof(page, proof);
    expect(sql(`SELECT count(*) FROM app.customer_auth_tokens WHERE tenant_id='${tenant}' AND consumed_at IS NOT NULL;`)).toBe(before);
    await accountSnapshot(page, 'verification-valid-proof');
    await page.getByRole('button', { name: 'Verify email', exact: true }).click();
    await expect(page.locator('[data-customer-verification] > p[role="status"]')).toContainText('Email verified.');
    expect(Number(sql(`SELECT count(*) FROM app.customer_auth_tokens WHERE tenant_id='${tenant}' AND consumed_at IS NOT NULL;`))).toBe(Number(before) + 1);
    await accountSnapshot(page, 'verification-success');
    await openProof(page, proof);
    await page.getByRole('button', { name: 'Verify email', exact: true }).click();
    await expect(page.locator('[data-customer-verification] > p[role="status"]')).toContainText('Verification could not be completed.');
    await accountSnapshot(page, 'verification-replayed');
    await page.goto(`${site}/account`); await credentials(page, email, password);
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(page.getByRole('link', { name: 'Order history', exact: true })).toBeVisible();
    await accountSnapshot(page, 'account-signed-in');
    await fault(page, `${customerBackend}/session`, 'GET', '503');
    await page.reload();
    await expect(page.getByRole('button', { name: 'Temporarily unavailable', exact: true })).toBeDisabled();
    await expect(page.getByRole('button', { name: 'Register', exact: true })).toHaveCount(0);
    await accountSnapshot(page, 'account-disabled');
    expect(await (await control(page, 'status')).json()).toMatchObject({ matched: true, status: 200 });
    await control(page, 'release');
    await page.reload();
    await expect(page.getByRole('button', { name: 'Sign out', exact: true })).toBeVisible();
    await page.getByRole('button', { name: 'Sign out', exact: true }).click();
    await expect(page.getByLabel('Email', { exact: true })).toBeVisible();
    await credentials(page, email, password);
    await fault(page, `${customerBackend}/session`, 'POST', '503');
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(page.getByRole('alert')).toContainText('We could not complete this request.');
    await page.locator('input[type="password"]').evaluate(input => { (input as HTMLInputElement).value = ''; });
    await accountSnapshot(page, 'account-request-failure');
  });

  test('real proof enabled, private, processing and DB-clock expiry', async ({ page }, info) => {
    await page.goto(`${site}/customer/verify`);
    await expect(page.getByRole('button', { name: 'Verify email', exact: true })).toBeDisabled();
    await accountSnapshot(page, 'verification-no-proof');
    await page.goto(`${site}/account`);
    await page.evaluate(() => location.assign('/customer/verify#token=invalid'));
    await expect(page).toHaveURL(`${site}/customer/verify`);
    await expect(page.getByRole('button', { name: 'Verify email', exact: true })).toBeDisabled();
    await accountSnapshot(page, 'verification-invalid-proof');
    const email = `visual-235-expiry-${info.project.name}@example.test`;
    const proof = await registerAccount(page, email, accountPassword());
    await openProof(page, proof);
    await accountSnapshot(page, 'verification-private');
    await page.getByRole('button', { name: 'Verify email', exact: true }).focus();
    await page.keyboard.press('Tab'); await page.keyboard.press('Shift+Tab');
    await expect(page.getByRole('button', { name: 'Verify email', exact: true })).toBeFocused();
    await accountSnapshot(page, 'verification-keyboard-focus');
    await accountSnapshot(page, 'verification-enabled');
    // Preserve constraints; only owned real challenges age, using DB time.
    sql(`UPDATE app.customer_auth_tokens SET created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour'
      WHERE tenant_id='${tenant}' AND email_normalized='${email}' AND consumed_at IS NULL;`);
    await fault(page, `${customerBackend}/verifications`, 'POST', 'hold');
    await page.getByRole('button', { name: 'Verify email', exact: true }).click();
    await expect.poll(async () => (await (await control(page, 'status')).json()).status).toBe(400);
    await expect(page.locator('[data-customer-verification]')).toHaveAttribute('aria-busy', 'true');
    await expect(page.locator('[data-customer-verification] button[aria-busy="true"]')).toBeDisabled();
    await accountSnapshot(page, 'verification-processing', true);
    await control(page, 'release');
    await expect(page.locator('[data-customer-verification] > p[role="status"]')).toContainText('Verification could not be completed.');
    await accountSnapshot(page, 'verification-expired');
    expect(sql(`SELECT count(*) FROM app.customers WHERE tenant_id='${tenant}' AND email_normalized='${email}' AND verified_at IS NOT NULL;`)).toBe('0');
    expect(await page.evaluate(value => document.documentElement.outerHTML.includes(value), proof)).toBe(false);
  });
});

// #230 uses a fresh tenant per data fixture: other visual suites cannot change
// its list, pagination cursor or empty state. Historical dates are never masked.
const staffSite = 'https://127.0.0.1:4350';
function staffOrders(count = 1, long = false) {
  const target = randomUUID(), product = randomUUID(), variant = randomUUID();
  const secondVariant = randomUUID();
  const ids = Array.from({ length: count }, () => randomUUID());
  const number = long ? 9007199254740991 : 723001;
  const code = long ? 'LONG-SKU-'.repeat(28) + 'END' : 'STAFF-CANVAS-TOTE';
  sql(`BEGIN; INSERT INTO app.tenants(id,name,settlement_currency) VALUES('${target}','Visual staff fixture','USD');
    INSERT INTO app.tenant_memberships(tenant_id,user_id,role)
      SELECT '${target}',id,'owner' FROM app.users WHERE email='visual-owner@example.test';
    SELECT set_config('app.tenant_id','${target}',true);
    INSERT INTO app.products(tenant_id,id,product_code,lifecycle_status) VALUES('${target}','${product}','staff-visual','ready');
    INSERT INTO app.product_variants(tenant_id,id,product_id,sku,option_key,lifecycle_status)
      VALUES('${target}','${variant}','${product}','${code}','default','ready');
    SELECT app.inventory_adjust('${variant}',1000,'Visual staff stock',gen_random_uuid(),'inbound');
    ${long ? `INSERT INTO app.product_variants(tenant_id,id,product_id,sku,option_key,lifecycle_status)
      VALUES('${target}','${secondVariant}','${product}','SECOND-LINE','second','ready');
      SELECT app.inventory_adjust('${secondVariant}',1,'Visual second stock',gen_random_uuid(),'inbound');` : ''}
    ${ids.map((id, index) => `DO $staff$ DECLARE r app.inventory_reservations; r2 app.inventory_reservations; BEGIN
      r:=app.inventory_reserve('${variant}',1,'order','${id}',gen_random_uuid()::text,3600);
      IF ${long} THEN r2:=app.inventory_reserve('${secondVariant}',1,'order','${id}',gen_random_uuid()::text,3600); END IF;
      SET LOCAL ROLE app_orders_executor;
      INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,
        items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor,created_at)
      VALUES('${target}','${id}',${number - index},gen_random_uuid()::text,sha256('${id}'::bytea),
        '{"email":"staff-visual@example.test"}','USD',${long ? '9223372036854775807' : '3140'},0,0,'{}',
        ${long ? '9223372036854775807' : '3140'},'2026-01-02T12:00:00Z'::timestamptz-${index}*interval '1 second');
      INSERT INTO app.order_lines(tenant_id,id,order_id,sku_id,sku,quantity,unit_price_minor,line_net_minor,
        line_tax_minor,line_total_minor,reservation_id,tax_snapshot)
      VALUES('${target}',${long ? "'00000001-0000-4000-8000-000000000001'" : "'" + id + "'"}::uuid,'${id}','${variant}','${code}',1,
        ${long ? '9223372036854775807' : '3140'},${long ? '9223372036854775807' : '3140'},0,
        ${long ? '9223372036854775807' : '3140'},r.id,'{}');
      IF ${long} THEN
        INSERT INTO app.order_lines(tenant_id,id,order_id,sku_id,sku,quantity,unit_price_minor,line_net_minor,
          line_tax_minor,line_total_minor,reservation_id,tax_snapshot)
        VALUES('${target}','ffffffff-ffff-4fff-bfff-ffffffffffff','${id}','${secondVariant}','SECOND-LINE',1,0,0,0,0,r2.id,'{}');
      END IF;
      UPDATE app.orders SET sealed=true,updated_at=created_at WHERE tenant_id='${target}' AND id='${id}';
      RESET ROLE;
    END $staff$;`).join('\n')}
    COMMIT;`);
  return { tenant: target, product, variant, ids, number: String(number), code,
    path: `/t/${target}/orders`, api: `/api/v1/t/${target}/orders` };
}
// Orders' existing real seed command owns reservation validation, snapshots,
// number allocation, sealing and outbox. Catalog API cannot create legacy spaced
// SKU text and Orders has no title snapshot; seed that catalog boundary only.
function domainMultilineOrder() {
  const fixture = staffOrders(0), id = randomUUID();
  const code = 'Canvas tote with reinforced shoulder straps and a roomy everyday compartment for books groceries and weekend travel essentials in natural cotton canvas';
  sql(`BEGIN; SELECT set_config('app.tenant_id','${fixture.tenant}',true);
    UPDATE app.product_variants SET sku='${code}' WHERE tenant_id='${fixture.tenant}' AND id='${fixture.variant}';
    DO $domain$ DECLARE r app.inventory_reservations; BEGIN
      r:=app.inventory_reserve('${fixture.variant}',1,'order','${id}',gen_random_uuid()::text,3600);
      PERFORM app.orders_create('${id}',gen_random_uuid()::text,sha256('${id}'::bytea),NULL,
        '{"email":"staff-visual@example.test"}','USD',0,'{}',
        (SELECT jsonb_build_array(jsonb_build_object('id',gen_random_uuid(),'sku_id',v.id,'sku',v.sku,
          'quantity',1,'unit_price_minor',3140,'line_net_minor',3140,'line_tax_minor',0,
          'line_total_minor',3140,'reservation_id',r.id,'tax_snapshot','{"category":"standard"}'::jsonb))
         FROM app.product_variants v WHERE v.tenant_id='${fixture.tenant}' AND v.id='${fixture.variant}'),'[]');
    END $domain$; COMMIT;`);
  return { ...fixture, ids: [id], number: '1', code };
}
async function staffLogin(page: Page, path: string, origin = staffSite) {
  await page.goto(origin + path);
  await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
  await page.getByLabel('Email', { exact: true }).fill('visual-owner@example.test');
  await page.getByLabel('Password', { exact: true }).fill(runtime().staffPassword);
  const signedIn = page.waitForResponse(r => new URL(r.url()).pathname === '/api/v1/auth/login' && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  expect((await signedIn).status()).toBe(201);
  await expect(page).toHaveURL(origin + path);
}
async function staffSnapshot(page: Page, state: typeof REAL_VISUAL_STATES[230][number], masks: Locator[] = []) {
  await page.evaluate(async () => { await document.fonts.ready; });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'staff fits viewport').toBe(true);
  const report = await new AxeBuilder({ page }).analyze();
  expect(report.violations.filter(item => ['serious', 'critical'].includes(item.impact ?? ''))).toEqual([]);
  await expect(page.getByText('CONTRACT FIXTURE', { exact: false })).toHaveCount(0);
  const maskColor = await page.locator('body').evaluate(node => getComputedStyle(node).backgroundColor);
  await expect(page).toHaveScreenshot(`real-230-${state}.png`, { fullPage: true, mask: masks, maskColor });
}
async function staffListReady(page: Page) {
  await expect(page.getByRole('table', { name: 'Order list', exact: true })).toHaveAttribute('aria-busy', 'false');
}

test.describe('VISUAL-REAL #230', () => {
  test.afterEach(async ({ page }) => { await control(page, 'release'); });

  test('real staff normal list/detail are read-only with exact amounts and persisted ETag', async ({ page }) => {
    const fixture = staffOrders();
    await staffLogin(page, fixture.path); await staffListReady(page);
    const writes: string[] = [];
    page.on('request', request => {
      if (new URL(request.url()).pathname.startsWith(fixture.api) && !['GET', 'HEAD', 'OPTIONS'].includes(request.method())) writes.push(request.method());
    });
    const listResponse = await page.request.get(staffSite + fixture.api);
    expect(listResponse.status()).toBe(200);
    expect((await listResponse.json()).items).toEqual([expect.objectContaining({ id: fixture.ids[0], order_number: 723001, total_minor: '3140', state: 'pending_payment' })]);
    await expect(page.getByRole('link', { name: fixture.number, exact: true })).toBeVisible();
    await staffSnapshot(page, 'list');
    const read = page.waitForResponse(r => new URL(r.url()).pathname === `${fixture.api}/${fixture.ids[0]}`);
    await page.getByRole('link', { name: fixture.number, exact: true }).click();
    const response = await read, detail = await response.json();
    expect(response.status()).toBe(200); expect(response.headers().etag).toBe(`"${detail.version}"`);
    expect(detail.lines).toEqual([expect.objectContaining({ sku_code: fixture.code, quantity: 1, line_total_minor: '3140' })]);
    expect(detail.total_minor).toBe('3140'); expect(detail).not.toHaveProperty('guest_contact');
    await expect(page.getByRole('heading', { name: `Order ${fixture.number}`, exact: true })).toBeVisible();
    await expect(page.getByRole('cell', { name: fixture.code, exact: true })).toBeVisible();
    const main = page.getByRole('main');
    await expect(main.getByRole('button')).toHaveCount(0); await expect(main.getByRole('form')).toHaveCount(0);
    await expect(main.locator('input,textarea,select,[contenteditable="true"]')).toHaveCount(0);
    await staffSnapshot(page, 'detail'); expect(writes).toEqual([]);
    console.log('VISUAL-230 write conflict/disabled mutation N/A: no admin order write UI (read-only contract #52)');
  });

  test('real staff long safe order number, bigint money and two snapshot lines stay in the scroll region', async ({ page }) => {
    const fixture = staffOrders(1, true);
    expect(fixture.code.length).toBe(255);
    await staffLogin(page, fixture.path); await staffListReady(page);
    await expect(page.getByRole('link', { name: fixture.number, exact: true })).toBeVisible();
    await staffSnapshot(page, 'long-list');
    await page.getByRole('link', { name: fixture.number, exact: true }).click();
    await expect(page.getByRole('heading', { name: `Order ${fixture.number}`, exact: true })).toBeVisible();
    const table = page.getByRole('table', { name: 'Order lines', exact: true });
    await expect(table.getByRole('row')).toHaveCount(3);
    await expect(table.getByRole('cell', { name: fixture.code, exact: true })).toBeVisible();
    await expect(table.getByRole('cell', { name: 'SECOND-LINE', exact: true })).toBeVisible();
    await expect(page.getByRole('main')).toContainText('USD 92,233,720,368,547,758.07');
    const region = page.getByRole('region', { name: 'Order lines', exact: true });
    await region.focus(); await expect(region).toBeFocused();
    await staffSnapshot(page, 'long-detail');
    if (await region.evaluate(el => el.scrollWidth > el.clientWidth)) {
      await page.keyboard.press('ArrowRight');
      await expect.poll(() => region.evaluate(el => el.scrollLeft)).toBeGreaterThan(0);
    }
  });

  test('real empty tenant and keyset pagination retain disabled and enabled navigation', async ({ page }) => {
    const empty = staffOrders(0);
    await staffLogin(page, empty.path); await staffListReady(page);
    await expect(page.getByText('No orders match this filter.', { exact: true })).toBeVisible();
    for (const name of ['Previous page', 'Next page']) await expect(page.getByRole('button', { name, exact: true })).toBeDisabled();
    await staffSnapshot(page, 'empty');
    const fixture = staffOrders(27);
    await page.goto(staffSite + fixture.path); await staffListReady(page);
    await expect(page.getByRole('table', { name: 'Order list' }).getByRole('row')).toHaveCount(26);
    const nextRead = page.waitForResponse(r => new URL(r.url()).pathname === fixture.api && new URL(r.url()).searchParams.has('cursor'));
    await page.getByRole('button', { name: 'Next page', exact: true }).click();
    const response = await nextRead; expect(response.status()).toBe(200);
    const next = await response.json(); expect(next.items).toHaveLength(2);
    expect(next.items.map((row: { id: string }) => row.id)).toEqual(fixture.ids.slice(25));
    await staffListReady(page);
    await expect(page.getByRole('button', { name: 'Previous page', exact: true })).toBeEnabled();
    await expect(page.getByRole('button', { name: 'Next page', exact: true })).toBeDisabled();
    await staffSnapshot(page, 'pagination');
    await page.getByRole('button', { name: 'Previous page', exact: true }).click(); await staffListReady(page);
    await expect(page.getByRole('link', { name: fixture.number, exact: true })).toBeVisible();
  });

  test('real staff loading and 503 reads expose safe retry without fixture fallback', async ({ page }) => {
    const fixture = staffOrders();
    await staffLogin(page, fixture.path); await staffListReady(page);
    for (const detail of [false, true]) {
      const api = fixture.api + (detail ? `/${fixture.ids[0]}` : '');
      const path = fixture.path + (detail ? `/${fixture.ids[0]}` : '');
      await fault(page, api, 'GET', 'hold'); await page.goto(staffSite + path);
      await expect.poll(async () => (await (await control(page, 'status')).json()).status).toBe(200);
      if (detail) await expect(page.getByRole('status')).toHaveText('Loading order…');
      else await expect(page.getByRole('table', { name: 'Order list' })).toHaveAttribute('aria-busy', 'true');
      await staffSnapshot(page, detail ? 'detail-loading' : 'list-loading');
      await control(page, 'release');
      if (detail) await expect(page.getByRole('heading', { name: `Order ${fixture.number}`, exact: true })).toBeVisible();
      else await staffListReady(page);
      await fault(page, api, 'GET', '503');
      const read = page.waitForResponse(r => new URL(r.url()).pathname === api);
      await page.reload(); expect((await read).status()).toBe(503);
      expect(await (await control(page, 'status')).json()).toMatchObject({ matched: true, status: 200 });
      await expect(page.getByRole('alert')).toHaveText('The service is temporarily unavailable. Try again later.Retry');
      await expect(page.getByRole('table')).toHaveCount(0);
      await staffSnapshot(page, detail ? 'order-detail-unavailable-503' : 'list-503');
      await control(page, 'release');
      await page.getByRole('button', { name: 'Retry', exact: true }).click();
      if (detail) await expect(page.getByRole('heading', { name: `Order ${fixture.number}`, exact: true })).toBeVisible();
      else await staffListReady(page);
      await expect(page.getByRole('alert')).toHaveCount(0);
    }
  });

  test('real staff guard, foreign order, viewer denial and DB-clock session expiry hide private data', async ({ page }) => {
    const fixture = staffOrders(), other = staffOrders();
    let reads = 0;
    page.on('request', r => { if (new URL(r.url()).pathname.startsWith(fixture.api)) reads++; });
    await page.goto(staffSite + fixture.path);
    await expect(page).toHaveURL(/\/login$/); await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
    expect(reads).toBe(0); await staffSnapshot(page, 'unauthenticated');
    await staffLogin(page, fixture.path); await staffListReady(page);
    const own = await page.request.get(`${staffSite}${other.api}/${other.ids[0]}`);
    expect(own.status()).toBe(200); // foreign id really exists; this staff owns both fixture tenants.
    const foreignRead = page.waitForResponse(r => new URL(r.url()).pathname === `${fixture.api}/${other.ids[0]}`);
    await page.goto(`${staffSite}${fixture.path}/${other.ids[0]}`);
    expect((await foreignRead).status()).toBe(404);
    await expect(page.getByRole('alert')).toContainText('Order not found or unavailable in this tenant.');
    await expect(page.getByRole('table')).toHaveCount(0); await staffSnapshot(page, 'cross-tenant');
    sql(`UPDATE app.tenant_memberships SET role='viewer' WHERE tenant_id='${fixture.tenant}'
      AND user_id=(SELECT id FROM app.users WHERE email='visual-owner@example.test');`);
    const denied = page.waitForResponse(r => new URL(r.url()).pathname === fixture.api);
    await page.goto(staffSite + fixture.path); expect((await denied).status()).toBe(403);
    await expect(page.getByRole('alert')).toContainText('This action is not allowed for your current session.');
    await expect(page.getByRole('table')).toHaveCount(0); await staffSnapshot(page, 'permission-denied');
    sql(`UPDATE app.tenant_memberships SET role='owner' WHERE tenant_id='${fixture.tenant}'
      AND user_id=(SELECT id FROM app.users WHERE email='visual-owner@example.test');`);
    await page.goto(`${staffSite}${fixture.path}/${fixture.ids[0]}`);
    await expect(page.getByRole('heading', { name: `Order ${fixture.number}`, exact: true })).toBeVisible();
    const session = await (await page.request.get(`${staffSite}/api/v1/auth/session`)).json();
    expect(session.session_id).toMatch(/^[0-9a-f-]{36}$/);
    sql(`WITH stamp AS MATERIALIZED (SELECT clock_timestamp() AS n)
      UPDATE app.auth_sessions SET created_at=stamp.n-interval '2 hours',last_seen_at=stamp.n-interval '1 hour',
        expires_at=stamp.n-interval '1 second' FROM stamp WHERE id='${session.session_id}';`);
    const expired = page.waitForResponse(r => new URL(r.url()).pathname === fixture.api);
    await page.getByRole('navigation', { name: 'Store navigation' }).getByRole('link', { name: 'Orders', exact: true }).click();
    expect((await expired).status()).toBe(401);
    await expect(page.getByRole('status')).toHaveText('Your session expired. Sign in to continue.');
    await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
    await expect(page.getByRole('table')).toHaveCount(0); await staffSnapshot(page, 'expired-session');
    expect(await page.evaluate(() => Object.keys(localStorage))).toEqual([]);
  });
});


test.describe('VISUAL-REAL #230 round5', () => {
  test('real multiline order text wraps completely inside the visible line cell', async ({ page }) => {
    const fixture = domainMultilineOrder();
    await staffLogin(page, `${fixture.path}/${fixture.ids[0]}`);
    const response = await page.request.get(`${staffSite}${fixture.api}/${fixture.ids[0]}`);
    expect(response.status()).toBe(200);
    const order = await response.json(), snapshot = order.lines[0];
    expect(snapshot.sku_code).toBe(sql(`SELECT sku FROM app.product_variants WHERE tenant_id='${fixture.tenant}' AND id='${fixture.variant}';`));
    expect(snapshot).toMatchObject({ title: null, quantity: 1, line_total_minor: '3140' });
    expect(sql(`SELECT count(*) FROM app.outbox_events WHERE tenant_id='${fixture.tenant}' AND dedup_key='created:${fixture.ids[0]}';`)).toBe('1');
    const cell = page.getByRole('cell', { name: fixture.code, exact: true });
    await expect(cell).toBeVisible();
    expect(await cell.innerText()).toBe(fixture.code);
    const geometry = await cell.evaluate(node => {
      const range = document.createRange(); range.selectNodeContents(node);
      const rects = [...range.getClientRects()].filter(r => r.width > 0 && r.height > 0);
      const region = node.closest('[role="region"]')!.getBoundingClientRect();
      return { lines: new Set(rects.map(r => r.top)).size,
        visible: rects.every(r => r.left >= region.left && r.right <= region.right),
        clipped: node.scrollHeight > node.clientHeight };
    });
    expect(geometry.lines, 'multi-word line item must genuinely wrap').toBeGreaterThan(1);
    expect(geometry.visible, 'all wrapped text stays visible without horizontal clipping').toBe(true);
    expect(geometry.clipped).toBe(false);
    const times = page.getByRole('main').locator('time');
    await expect(times).toHaveCount(2);
    for (const [index, stamp] of [order.created_at, order.updated_at].entries()) {
      await expect(times.nth(index)).toHaveAttribute('datetime', stamp);
      await expect(times.nth(index)).toHaveText(stamp);
    }
    const metadata = times.first().locator('..');
    await expect(metadata).toHaveText(`Created ${order.created_at} · Updated ${order.updated_at}`);
    await staffSnapshot(page, 'multiline', [await runtimeMask(page, metadata, metadata)]);
  });
});

// Masks own only a verified runtime metadata field or UUID cell; status/version/money stay compared.
async function runtimeMask(page: Page, target: Locator, field: Locator) {
  const box = await target.boundingBox();
  const fieldBounds = await field.boundingBox();
  if (!box || !fieldBounds) throw new Error('Missing runtime mask geometry');
  const protectedBoxes: Box[] = [];
  for (const locator of [page.getByRole('heading'), page.locator('.os-badge').locator('..'), page.locator('.admin-stack dd'),
    page.locator('[data-completion-ready] dd').filter({ hasNotText: /^[0-9a-f-]{36}$/ })]) {
    for (const element of await locator.all()) {
      const bounds = await element.boundingBox(); if (bounds) protectedBoxes.push(bounds);
    }
  }
  assertDateMaskBounds(box, fieldBounds, protectedBoxes);
  return target;
}
// This candidate extends only one pixel beyond its independently measured field.
// It need not touch money/status: containment itself must reject it.
async function assertOversizedRuntimeMaskRejected(page: Page, field: Locator) {
  const bounds = await field.boundingBox();
  if (!bounds) throw new Error('Missing runtime field geometry');
  await page.evaluate(box => {
    const probe = document.createElement('div');
    probe.dataset.visualMaskProbe = '';
    Object.assign(probe.style, { position: 'fixed', left: `${box.x}px`, top: `${box.y}px`,
      width: `${box.width + 1}px`, height: `${box.height}px`, pointerEvents: 'none' });
    document.body.append(probe);
  }, bounds);
  try {
    await expect(runtimeMask(page, page.locator('[data-visual-mask-probe]'), field))
      .rejects.toThrow('Date mask extends outside time element');
    console.info('VISUAL-230 expected RED: oversized runtime mask rejected against independent field bounds');
  } finally {
    await page.locator('[data-visual-mask-probe]').evaluate(node => node.remove());
  }
}
// Exercise different legal glyph widths/precisions in CI, then restore the real API text.
// A broad mask is separately rejected against the protected status/money geometry.
async function assertStableRuntimeMask(page: Page, masks: Locator[], field: Locator, values: Locator, variants: string[][]) {
  await page.evaluate(async () => { await document.fonts.ready; });
  const original = await values.allTextContents();
  const maskColor = await page.locator('body').evaluate(node => getComputedStyle(node).backgroundColor);
  const capture = () => page.screenshot({ fullPage: true, mask: masks, maskColor, animations: 'disabled', caret: 'hide' });
  const before = await capture();
  try {
    for (const variant of variants) {
      expect(variant).toHaveLength(original.length);
      for (const [index, text] of variant.entries()) await values.nth(index).evaluate((node, value) => { node.textContent = value; }, text);
      for (const mask of masks) await runtimeMask(page, mask, field);
      const after = await capture();
      if (!after.equals(before)) {
        await test.info().attach('runtime-mask-before', { body: before, contentType: 'image/png' });
        await test.info().attach('runtime-mask-after', { body: after, contentType: 'image/png' });
      }
      expect(after.equals(before), 'runtime field changes must not alter any unmasked pixel').toBe(true);
    }
  } finally {
    for (const [index, text] of original.entries()) await values.nth(index).evaluate((node, value) => { node.textContent = value; }, text);
  }
}
async function domainSnapshot(page: Page, state: typeof REAL_VISUAL_STATES[230][number], order: {
  state: string; order_number: number; version: number; created_at: string; updated_at: string;
}) {
  await page.reload();
  await expect(page.getByRole('heading', { name: `Order ${order.order_number}`, exact: true })).toBeVisible();
  await expect(page.locator('.os-badge')).toHaveText(order.state.replaceAll('_', ' '));
  await expect(page.getByRole('main')).toContainText(`Version ${order.version}`);
  await expect(page.getByRole('main')).toContainText('USD 57.80');
  const times = page.getByRole('main').locator('time');
  await expect(times).toHaveCount(2);
  for (const [index, stamp] of [order.created_at, order.updated_at].entries()) {
    await expect(times.nth(index)).toHaveAttribute('datetime', stamp);
    await expect(times.nth(index)).toHaveText(stamp);
  }
  // Both inline times affect the position of the fixed Updated label. Mask their
  // exact metadata field, verified to contain only those two timestamps/labels.
  const metadata = times.first().locator('..');
  expect(await metadata.evaluate(node => node.tagName)).toBe('P');
  await expect(metadata.locator(':scope > *')).toHaveCount(2);
  await expect(metadata).toHaveText(`Created ${order.created_at} · Updated ${order.updated_at}`);
  const masks = [await runtimeMask(page, metadata, metadata)];
  if (state === 'paid') {
    await expect(runtimeMask(page, page.getByRole('main'), metadata)).rejects.toThrow('Date mask extends outside time element');
    await assertOversizedRuntimeMaskRejected(page, metadata);
    await page.evaluate(async () => { await document.fonts.ready; });
    // Inject the former narrow-time mask: changing precision MUST expose its
    // unmasked Updated-label movement before the repaired field mask is tested.
    const narrow = await times.all();
    const maskColor = await page.locator('body').evaluate(node => getComputedStyle(node).backgroundColor);
    const beforeNarrow = await page.screenshot({ fullPage: true, mask: narrow, maskColor });
    try {
      const changed = order.created_at.includes('.') ? order.created_at.replace(/\.\d+Z$/, 'Z') : order.created_at.replace('Z', '.123456Z');
      await times.first().evaluate((node, value) => { node.textContent = value; }, changed);
      const afterNarrow = await page.screenshot({ fullPage: true, mask: narrow, maskColor });
      expect(afterNarrow.equals(beforeNarrow), 'fault witness: narrow time mask must be rejected by pixel invariance').toBe(false);
      console.info('VISUAL-230 expected RED: narrow time mask moves the unmasked Updated label');
    } finally {
      await times.first().evaluate((node, value) => { node.textContent = value; }, order.created_at);
    }
    await assertStableRuntimeMask(page, masks, metadata, times, [
      ['2099-12-31T23:59:59.999999Z', '2099-12-31T23:59:59.999999Z'],
      ['2000-01-01T00:00:00Z', '2000-01-01T00:00:00Z'],
    ]);
    await expect(metadata).toHaveText(`Created ${order.created_at} · Updated ${order.updated_at}`);
  }
  await staffSnapshot(page, state, masks);
}

test('VISUAL-REAL #230 real two-unit purchase and five staff domain states', async ({ page }, info) => {
  test.setTimeout(300_000);
  const site = 'https://localhost:4351', staffSite = 'https://localhost:4352';
  const tenant = 'c2300001-0000-4000-8000-000000000001';
  // Same quantity-two journey as tests/e2e/rma.spec.ts, on its isolated REAL tenant.
  await page.goto(`${site}/products/canvas-tote`);
  await page.getByRole('button', { name: 'Add to cart', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('Item added to cart.');
  await page.goto(`${site}/cart`);
  await expect(page.locator('[data-cart-line]')).toHaveCount(1);
  await page.getByLabel('Quantity', { exact: true }).fill('2');
  await page.getByRole('button', { name: 'Update quantity', exact: true }).click();
  await expect(page.getByRole('main')).toContainText('$48.00');
  await expect(page.getByLabel('Quantity', { exact: true })).toHaveValue('2');
  await staffSnapshot(page, 'rma-cart');
  await page.goto(`${site}/checkout`);
  for (const [label, value] of [['Email', 'visual-rma@example.test'], ['Country code', 'US'],
    ['Address line 1', '1 Test Way'], ['City', 'Test City'], ['Postal code', '10001']]) {
    await page.getByLabel(label!, { exact: true }).fill(value!);
  }
  await page.getByRole('button', { name: 'Review totals', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Start test checkout', exact: true })).toBeEnabled();
  await expect(page.getByRole('main')).toContainText('$57.80');
  await staffSnapshot(page, 'rma-checkout');
  const started = page.waitForResponse(r => new URL(r.url()).pathname === '/purchase/api/checkout' && r.request().method() === 'POST');
  await start(page);
  const response = await started;
  expect(response.status()).toBe(201);
  const checkout = await response.json();
  expect(checkout).toMatchObject({ state: 'ready', amount_minor: '5780', currency: 'USD' });
  await page.getByRole('button', { name: 'Fake payment: capture', exact: true }).click();
  await page.getByRole('link', { name: 'View order confirmation', exact: true }).click();
  await expect(page.locator('[data-completion-ready]')).toBeVisible();
  expect(await (await page.request.get(`${site}/purchase/api/completion`)).json())
    .toMatchObject({ order_id: checkout.order_id, state: 'paid', payment: { status: 'captured' } });

  const base = `${staffSite}/api/v1/t/${tenant}`;
  const orderPath = `/orders/${checkout.order_id}`;
  const completionID = page.locator('[data-completion-ready] .purchase-totals > div').first().locator('dd');
  await expect(completionID).toHaveText(checkout.order_id);
  // Browser glyph ink may overhang the UUID cell by a subpixel. Clip only the
  // already-masked value to its own bounds; no layout or unmasked UI is changed.
  const originalClip = await completionID.evaluate(node => {
    const element = node as HTMLElement, previous = element.style.clipPath;
    element.style.clipPath = 'inset(0)'; return previous;
  });
  try {
    const masks = [await runtimeMask(page, completionID, completionID)];
    await expect(runtimeMask(page, page.getByRole('main'), completionID)).rejects.toThrow('Date mask extends outside time element');
    await assertOversizedRuntimeMaskRejected(page, completionID);
    await assertStableRuntimeMask(page, masks, completionID, completionID, [
      [checkout.order_id.slice(0, -1) + '0'], [checkout.order_id.slice(0, -1) + '8'],
    ]);
    await expect(completionID).toHaveText(checkout.order_id);
    await staffSnapshot(page, 'rma-completion', masks);
  } finally {
    await completionID.evaluate((node, value) => { (node as HTMLElement).style.clipPath = value; }, originalClip);
  }
  await staffLogin(page, `/t/${tenant}${orderPath}`, staffSite);
  await expect(page.getByRole('cell', { name: 'C24-CANVAS-TOTE', exact: true })).toBeVisible();
  const session = await (await page.request.get(`${staffSite}/api/v1/auth/session`)).json();
  const read = async () => {
    const response = await page.request.get(base + orderPath);
    expect(response.status()).toBe(200);
    return response.json();
  };
  const write = (path: string, data: unknown, version: number, key = crypto.randomUUID(), targetTenant: string = tenant) =>
    page.request.post(`${staffSite}/api/v1/t/${targetTenant}${path}`, { data, headers: {
      Origin: staffSite, 'X-CSRF-Token': session.csrf_token,
      'Idempotency-Key': key, 'If-Match': `"${version}"`,
    } });
  const accepted = async (op: string, response: APIResponse, status: number) => {
    let body;
    try { body = await response.json(); }
    catch { throw new Error(`${op}: status=${response.status()} code=invalid_json`); }
    const code = body.code === 'route_not_found' ? 'route_not_found' : response.ok() ? 'none' : 'unknown';
    const label = `${op}: status=${response.status()} code=${code}`;
    if (response.status() === 503 || body.code === 'route_not_found' ||
        (op === 'refundReturn' && response.status() !== status))
      throw new Error(label);
    expect(response.status(), 'Return operation status').toBe(status);
    return body;
  };
  let order = await read();
  expect(order).toMatchObject({ state: 'paid', total_minor: '5780', lines: [{ quantity: 2 }] });
  await domainSnapshot(page, 'paid', order);
  const line = order.lines[0].line_id;
  const shipment = (n: number) => ({ carrier: 'E2E-RMA', tracking_number: `visual-rma-${info.project.name}-${n}`,
    lines: [{ order_line_id: line, quantity: 1 }] });
  const first = await accepted('createShipment', await write(orderPath + '/shipments', shipment(1), order.version), 201);
  order = await read();
  expect(order.state).toBe('partially_fulfilled');
  expect(order.version).toBe(first.order_version);
  await domainSnapshot(page, 'partially-fulfilled', order);
  const second = await accepted('createShipment', await write(orderPath + '/shipments', shipment(2), order.version), 201);
  order = await read();
  expect(order.state).toBe('fulfilled');
  expect(order.version).toBe(second.order_version);
  await domainSnapshot(page, 'fulfilled', order);
  expect(second.id).not.toBe(first.id);

  const requestReturn = { kind: 'shipment_return', reason: 'E2E-RMA', lines: [
    { order_line_id: line, shipment_line_id: first.lines[0].id, quantity: 1 },
  ] };
  const rma = await accepted('createReturn', await write(orderPath + '/returns', requestReturn, order.version), 201);
  expect(rma).toMatchObject({ status: 'requested', restocked: false, refund_minor: '2890' });
  const returnPath = `/returns/${rma.id}`;
  const receiptKey = crypto.randomUUID();
  const received = await accepted('receiveReturn', await write(returnPath + '/receive', { restock: true }, rma.order_version, receiptKey), 200);
  expect(received).toMatchObject({ id: rma.id, status: 'received', restocked: true });
  const replay = await accepted('receiveReturn', await write(returnPath + '/receive', { restock: true }, rma.order_version, receiptKey), 200);
  expect(replay).toEqual(received);
  // A fresh command identity cannot receive/restock the same RMA again.
  const duplicate = await write(returnPath + '/receive', { restock: true }, received.order_version);
  expect([409, 422]).toContain(duplicate.status());
  const foreignTenant = sql(`SELECT tenant_id FROM app.tenant_memberships WHERE user_id=(SELECT id FROM app.users WHERE email='visual-owner@example.test') AND tenant_id<>'${tenant}' ORDER BY tenant_id LIMIT 1;`);
  const foreign = await write(returnPath + '/receive', { restock: true }, received.order_version,
    crypto.randomUUID(), foreignTenant);
  expect(foreign.status()).toBe(404);
  expect((await page.request.get(`${staffSite}/api/v1/t/${foreignTenant}${orderPath}`)).status()).toBe(404);
  // Money is server owned: injected refund amount is rejected before any PSP action.
  expect((await write(returnPath + '/refund', { amount_minor: '5781' }, received.order_version)).status()).toBe(400);
  const refundKey = crypto.randomUUID();
  const refunded = await accepted('refundReturn', await write(returnPath + '/refund', {}, received.order_version, refundKey), 200);
  expect(refunded).toMatchObject({ id: rma.id, status: 'refunded', restocked: true, refund_minor: '2890' });
  expect(await accepted('refundReturn', await write(returnPath + '/refund', {}, received.order_version, refundKey), 200)).toEqual(refunded);
  order = await read();
  expect(order.state).toBe('partially_refunded');
  await domainSnapshot(page, 'partially-refunded', order);
  // Returning an already returned shipment unit would exceed its original entitlement.
  const excess = await write(orderPath + '/returns', requestReturn, order.version);
  expect([409, 422]).toContain(excess.status());
  expect(await read()).toEqual(order);
  // A distinct shipped unit needs its own RMA and Payments command identity.
  const remaining = await accepted('createReturn', await write(orderPath + '/returns', {
    ...requestReturn, lines: [{ order_line_id: line, shipment_line_id: second.lines[0].id, quantity: 1 }],
  }, order.version), 201);
  expect(remaining).toMatchObject({ status: 'requested', restocked: false, refund_minor: '2890' });
  expect(remaining.id).not.toBe(rma.id);
  const remainingPath = `/returns/${remaining.id}`;
  const remainingReceived = await accepted('receiveReturn', await write(remainingPath + '/receive',
    { restock: true }, remaining.order_version), 200);
  expect(remainingReceived).toMatchObject({ id: remaining.id, status: 'received', restocked: true });
  const fullRefundKey = crypto.randomUUID();
  expect(fullRefundKey).not.toBe(refundKey);
  const fullRefund = await accepted('refundReturn', await write(remainingPath + '/refund', {},
    remainingReceived.order_version, fullRefundKey), 200);
  expect(fullRefund).toMatchObject({ id: remaining.id, status: 'refunded', restocked: true, refund_minor: '2890' });
  expect(await accepted('refundReturn', await write(remainingPath + '/refund', {},
    remainingReceived.order_version, fullRefundKey), 200)).toEqual(fullRefund);
  order = await read();
  expect(order).toMatchObject({ state: 'refunded', total_minor: '5780' });
  expect(BigInt(refunded.refund_minor) + BigInt(fullRefund.refund_minor)).toBe(BigInt(order.total_minor));
  expect(await read()).toEqual(order);
  await page.reload();
  await expect(page.getByRole('main')).toContainText('refunded');
  await domainSnapshot(page, 'refunded', order);
  await info.attach('rma-journey', { body: JSON.stringify({ first, second, received, refunded, remainingReceived, fullRefund, order }), contentType: 'application/json' });
});

// VISUAL-274 structural regression: the checker owns locator resolution and geometry.
test('VISUAL-274 rejects oversized masks and aliases using independent DOM bounds', async ({ page }) => {
  await page.goto(site + '/account');
  await page.evaluate(() => {
    const field = document.createElement('div'); field.dataset.maskField = '';
    Object.assign(field.style, { position: 'fixed', left: '20px', top: '20px', width: '100px', height: '20px' });
    const mask = document.createElement('span'); mask.dataset.maskCandidate = '';
    Object.assign(mask.style, { display: 'block', width: '101px', height: '20px' });
    field.append(mask); document.body.append(field);
  });
  const field = page.locator('[data-mask-field]'), mask = page.locator('[data-mask-candidate]');
  // Cast only bridges the old box API for the tests-first commit; production repair removes it.
  const check = assertDateMaskBounds as unknown as (mask: Locator, bounds: Locator, protectedBoxes: Box[]) => Promise<void>;
  await expect(check(mask, field, [])).rejects.toThrow('Date mask extends outside time element');
  await mask.evaluate(node => { (node as HTMLElement).style.width = '100px'; });
  await check(mask, field, []);
  await expect(check(field, field, [])).rejects.toThrow('Mask and bounds resolve to the same element');
  await page.locator('[data-mask-field]').evaluate(node => node.remove());
});
