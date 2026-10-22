/** Responsibility: expose whole MVP browser journeys that current main cannot execute.
 * Dependencies: real pending feature PRs; caller: CI E2E and acceptance matrix.
 * Invariants: FIXME is never PASS; removing it requires a real scenario, not an empty body.
 */
import { expect, test } from '@playwright/test';
import { audit, fixture } from './fixture';

test('FIXME staff create SKU → media upload → publish', async () => {
  test.fixme(true, 'Staff/catalog/media/CMS authorization is merged; catalog content/listing/price write APIs are absent. Await the explicit E2E-ADMIN fixture-preparation decision before claiming a created SKU reaches publication.');
  throw new Error('Replace this gap with the complete real browser journey and persisted API assertions before removing FIXME');
});

test('real admin orders list and detail use persisted orders and staff scopes', async ({ page }, info) => {
  const suffix = info.project.name === 'desktop' ? '000000000001' : '000000000002';
  const tenant = `c1900010-0000-4000-8000-${suffix}`;
  const orderId = `c1900003-0000-4000-8000-${suffix}`;
  const base = `${fixture.admin}/api/v1/t/${tenant}/orders`;
  // The production route guard must deny a direct private orders URL before reads.
  await page.goto(`${fixture.admin}/t/${tenant}/orders`);
  await expect(page).toHaveURL(`${fixture.admin}/login`);
  await page.getByLabel('Email', { exact: true }).fill(fixture.ownerEmail);
  await page.getByLabel('Password', { exact: true }).fill(fixture.ownerPassword);
  const listed = page.waitForResponse(r => r.url().startsWith(base + '?') && r.request().method() === 'GET');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('table', { name: 'Order list' })).toBeVisible();
  const response = await listed;
  expect(response.status()).toBe(200);
  const list = await response.json();
  expect(list.items).toEqual([expect.objectContaining({ id: orderId, state: 'pending_payment',
    total_minor: '270', currency: 'USD', customer_kind: 'guest' })]);
  await expect(page.getByText('CONTRACT FIXTURE')).toHaveCount(0);
  await audit(page, info);
  const detailRead = page.waitForResponse(r => r.url() === `${base}/${orderId}`);
  await page.getByRole('link', { name: String(list.items[0].order_number), exact: true }).click();
  const detail = await detailRead;
  expect(detail.status()).toBe(200);
  const order = await detail.json();
  expect(detail.headers().etag).toBe(`"${order.version}"`);
  expect(order).toMatchObject({ id: orderId, state: 'pending_payment', total_minor: '270', currency: 'USD',
    lines: [expect.objectContaining({ sku_code: 'C19-WEBHOOK', quantity: 2, line_total_minor: '220' })] });
  expect(order).not.toHaveProperty('guest_contact');
  await expect(page.getByRole('heading', { name: `Order ${order.order_number}`, exact: true })).toBeVisible();
  await expect(page.getByRole('cell', { name: 'C19-WEBHOOK', exact: true })).toBeVisible();
  await expect(page.getByText('USD 2.70', { exact: true })).toBeVisible();
  await audit(page, info);
  // Membership in this tenant cannot expose an order through a different tenant path.
  expect((await page.request.get(`${fixture.admin}/api/v1/t/${fixture.ownerTenantId}/orders/${orderId}`)).status()).toBe(404);
  expect((await page.request.get(`${fixture.admin}/api/v1/t/${fixture.tenantId}/orders`)).status()).toBe(404);
  const loggedOut = page.waitForResponse(r => r.url() === `${fixture.admin}/api/v1/auth/logout` && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  expect((await loggedOut).status()).toBe(204);
  await expect(page).toHaveURL(`${fixture.admin}/login`);
  expect((await page.request.get(base)).status()).toBe(401);
  await info.attach('admin-order-read', { body: JSON.stringify({ tenant, list, order }), contentType: 'application/json' });
});

