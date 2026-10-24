/** Real registered purchase → privacy export/worker → one-use download → erasure.
 * Depends on production HTTP, shared purchase UI and the deployed signed worker.
 * No privacy UI commands exist: exercise the staff API and audit #303 support UI.
 * SQL witnesses in run.py are read-only; no clock or business-state fixture writes.
 */
import { writeFile } from 'node:fs/promises';
import { test, expect, type APIResponse } from '@playwright/test';
import { fixture, publishedSite, audit, rememberSensitive } from './fixture';
import { start, registerAndLogin } from './purchase-helpers';

// Credentials/capabilities must never enter automatic capture artifacts.
test.use({ trace: 'off', video: 'off', screenshot: 'off' });

test('real registered customer export and erasure journey', async ({ page, request }, info) => {
  test.setTimeout(1_100_000); // Real DB capability TTL is 15 minutes, never shortened.
  await publishedSite(request);
  const email = `e2edr-${info.project.name}@example.test`;
  await registerAndLogin(page, email);
  const customerSession = await page.request.get(`${fixture.api}/api/v1/t/${fixture.tenantId}/customer/session`);
  expect(customerSession.status()).toBe(200);
  const customer = (await customerSession.json()).customer.customer_id;
  expect(customer).toMatch(/^[a-f0-9-]{36}$/);
  const checkout = await start(page, true);
  await page.getByRole('button', { name: 'Fake payment: capture', exact: true }).click();
  await page.getByRole('link', { name: 'View order confirmation', exact: true }).click();
  await expect(page.locator('[data-completion-ready]')).toBeVisible();
  const history = `${fixture.api}/api/v1/t/${fixture.tenantId}/customer/orders`;
  expect((await (await page.request.get(history)).json()).items).toHaveLength(1);
  const oldCustomerSession = (await page.context().cookies()).find(c => c.name === '__Host-osc_customer_session');
  expect(oldCustomerSession).toBeDefined();
  const base = `${fixture.admin}/api/v1/t/${fixture.tenantId}/customers/${customer}`;
  let csrf = '';
  const login = async (email: string) => {
    // Use the shipped logout route to rotate any prior staff session reliably.
    await page.goto(`${fixture.admin}/t/${fixture.tenantId}/data-privacy`);
    const signout = page.getByRole('button', { name: 'Sign out', exact: true });
    if (csrf) await signout.click();
    await page.getByLabel('Email', { exact: true }).fill(email);
    await page.getByLabel('Password', { exact: true }).fill(fixture.ownerPassword);
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(page.getByRole('button', { name: 'Sign out', exact: true })).toBeVisible();
    csrf = (await (await page.request.get(`${fixture.admin}/api/v1/auth/session`)).json()).csrf_token;
  };
  const post = (path: string) => page.request.post(path, { headers: {
    Origin: fixture.admin, 'X-CSRF-Token': csrf, 'Idempotency-Key': crypto.randomUUID(),
  } });
  const accepted = async (operation: string, response: APIResponse, status: number) => {
    const body = await response.json();
    if (body.download_url) {
      let token = '';
      try { token = new URL(body.download_url, fixture.admin).searchParams.get('token') ?? ''; }
      catch { throw new Error('Invalid export capability URL'); }
      await rememberSensitive(token);
    }
    if (response.status() === 503 || body.code === 'route_not_found')
      throw new Error(`BLOCKED ${operation} request_id=${response.headers()['x-request-id']}: ${response.status()} ${JSON.stringify(body)}`);
    expect(response.status(), `${operation}: ${JSON.stringify(body)}`).toBe(status);
    return body;
  };
  for (const role of ['catalog_editor', 'viewer']) {
    await login(`datarights-${role}@example.test`);
    expect((await post(base + '/data-exports')).status()).toBe(403);
    expect((await post(base + '/data-erasure')).status()).toBe(403);
  }
  await login('datarights-owner@example.test');
  await page.goto(`${fixture.admin}/t/${fixture.tenantId}/data-privacy`);
  await expect(page.getByText('To delete shop or customer data, contact support. We will process it within 30 days and provide an export copy.', { exact: true })).toBeVisible();
  await expect(page.getByText('如需删除店铺或客户数据，请联系支持，我们会在 30 天内处理并提供导出副本。', { exact: true })).toBeVisible();
  await audit(page, info);
  const foreign = base.replace(fixture.tenantId, fixture.ownerTenantId);
  expect((await post(foreign + '/data-exports')).status()).toBe(404);
  expect((await post(foreign + '/data-erasure')).status()).toBe(404);
  const before = await accepted('getOrder', await page.request.get(`${fixture.admin}/api/v1/t/${fixture.tenantId}/orders/${checkout.order_id}`), 200);
  const ready = async () => {
    const created = await accepted('requestCustomerDataExport', await post(base + '/data-exports'), 202);
    const path = base + '/data-exports/' + created.request_id;
    let view: {status: string; download_url: string; download_expires_at: string};
    await expect.poll(async () => {
      view = await accepted('getCustomerDataExport', await page.request.get(path), 200);
      return view.status;
    }, { timeout: 90_000 }).toBe('ready');
    const link = new URL(view!.download_url, fixture.admin);
    expect(link.origin).toBe(fixture.admin);
    expect(link.pathname).toBe(new URL(path).pathname + '/download');
    expect(link.searchParams.get('token')).toMatch(/^[A-Za-z0-9_-]{43}$/);
    return { path, link: link.href, expires: view!.download_expires_at, id: created.request_id };
  };
  const first = await ready();
  throw new Error('FIX-374 old assertion probe: stop after actual ready capability');
  expect((await page.request.get(first.path.replace(fixture.tenantId, fixture.ownerTenantId))).status()).toBe(404);
  expect((await post(first.link.replace(fixture.tenantId, fixture.ownerTenantId))).status()).toBe(404);
  const download = await post(first.link);
  expect(download.status()).toBe(200);
  expect(download.headers()['cache-control']).toBe('no-store');
  const exported = JSON.parse((await download.body()).toString());
  expect(exported.customer).toMatchObject({ id: customer, email });
  expect(exported.orders).toEqual(expect.arrayContaining([expect.objectContaining({ id: checkout.order_id, state: 'paid', total_minor: 3140 })]));
  expect(JSON.stringify(exported)).not.toMatch(/password_hash|token_hash|csrf_token/);
  // A second successful download must fail this gate even if metadata says consumed.
  expect((await post(first.link)).status()).toBe(409);
  expect((await (await page.request.get(first.path)).json()).status).toBe('consumed');
  const expiring = await ready();
  await test.step('wait for real 15-minute DB capability expiry', async () => {
    // Status is read-only and keeps the one-minute staff idle session active.
    // No download occurs until the server withholds the expired capability.
    await expect.poll(async () => {
      expect((await page.request.get(history)).status()).toBe(200);
      const response = await page.request.get(expiring.path);
      const status = await accepted('getCustomerDataExport', response, 200);
      return !status.download_url && Date.parse(response.headers().date) >= Date.parse(expiring.expires);
    }, { timeout: 960_000, intervals: [15_000] }).toBe(true);
  });
  expect((await post(expiring.link)).status()).toBe(409);
  const erased = await accepted('eraseCustomerData', await post(base + '/data-erasure'), 200);
  expect(erased.counts).toMatchObject({ credentials: 1, orders: 1, order_lines: 1, checkouts: 1 });
  expect(erased.revoked_exports.sort()).toEqual([first.id, expiring.id].sort());
  expect((await post(first.link)).status()).toBe(409);
  const after = await accepted('getOrder', await page.request.get(`${fixture.admin}/api/v1/t/${fixture.tenantId}/orders/${checkout.order_id}`), 200);
  for (const key of ['id', 'state', 'version', 'currency', 'total_minor', 'refunded_minor', 'lines']) {
    // Line tax snapshots are intentionally scrubbed, financial facts remain.
    if (key !== 'lines') expect(after[key]).toEqual(before[key]);
  }
  expect(after.lines.map((l: {line_id: string; quantity: number; line_total_minor: string}) => [l.line_id, l.quantity, l.line_total_minor]))
    .toEqual(before.lines.map((l: {line_id: string; quantity: number; line_total_minor: string}) => [l.line_id, l.quantity, l.line_total_minor]));
  await page.context().addCookies([oldCustomerSession!]);
  expect((await page.request.get(history)).status()).toBe(401);
  await page.goto(`${fixture.storefront}/account`);
  await page.getByLabel('Email', { exact: true }).fill(email);
  await page.getByLabel('Password', { exact: true }).fill('CI-only-c09f2-password!');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('alert')).toBeVisible();
  await expect(page.getByRole('link', { name: 'Order history', exact: true })).toHaveCount(0);
  // Only opaque IDs/counts are persisted; capabilities and export bytes stay private.
  await writeFile(`test-results/e2e/datarights-${info.project.name}.json`, JSON.stringify({
    project: info.project.name, customer, order: checkout.order_id, exports: [first.id, expiring.id], counts: erased.counts,
  }));
});
