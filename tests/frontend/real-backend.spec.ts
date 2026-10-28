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
  await expect(page).toHaveScreenshot(`real-235-${state}.png`, { fullPage: true });
}
const customerBackend = `/api/v1/t/${tenant}/customer`;
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
    await expect(page.getByRole('status')).toContainText('Email verified.');
    expect(Number(sql(`SELECT count(*) FROM app.customer_auth_tokens WHERE tenant_id='${tenant}' AND consumed_at IS NOT NULL;`))).toBe(Number(before) + 1);
    await accountSnapshot(page, 'verification-success');
    await openProof(page, proof);
    await page.getByRole('button', { name: 'Verify email', exact: true }).click();
    await expect(page.getByRole('status')).toContainText('Verification could not be completed.');
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
    await expect(page.getByRole('button', { name: 'Verify email', exact: true })).toBeDisabled();
    await accountSnapshot(page, 'verification-processing', true);
    await control(page, 'release');
    await expect(page.getByRole('status')).toContainText('Verification could not be completed.');
    await accountSnapshot(page, 'verification-expired');
    expect(sql(`SELECT count(*) FROM app.customers WHERE tenant_id='${tenant}' AND email_normalized='${email}' AND verified_at IS NOT NULL;`)).toBe('0');
    expect(await page.evaluate(value => document.documentElement.outerHTML.includes(value), proof)).toBe(false);
  });
});
