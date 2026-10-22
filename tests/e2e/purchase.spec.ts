/** Responsibility: real CMS publication -> browser purchase -> signed Payments -> persisted Checkout.
 * Dependencies: production Astro BFF, opt-in bootstrap composition, CI-only owned seed.
 * Invariants: no interception/fabricated receipt; pending receipt alone never means paid.
 */
import { createHmac } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { audit, fixture, publishedSite } from './fixture';

const api = `${fixture.storefront}/purchase/api`;
async function start(page: Page) {
  await page.goto(`${fixture.storefront}/products/${fixture.productSlug}`);
  await expect(page.getByRole('button', { name: 'Add to cart', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Add to cart', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('Item added to cart.');
  const cookie = (await page.context().cookies()).find(c => c.name === '__Host-osc_storefront_cart');
  expect(cookie && { secure: cookie.secure, httpOnly: cookie.httpOnly, sameSite: cookie.sameSite, path: cookie.path })
    .toEqual({ secure: true, httpOnly: true, sameSite: 'Lax', path: '/' });
  expect(await page.evaluate(() => document.cookie)).not.toContain('__Host-osc_storefront_cart');
  await page.goto(`${fixture.storefront}/cart`);
  await expect(page.locator('[data-cart-line]')).toHaveCount(1);
  await expect(page.getByRole('main')).toContainText('$24.00');
  await page.goto(`${fixture.storefront}/checkout`);
  await page.getByLabel('Email', { exact: true }).fill('c09d-buyer@example.test');
  await page.getByLabel('Country code', { exact: true }).fill('US');
  await page.getByLabel('Address line 1', { exact: true }).fill('1 Test Way');
  await page.getByLabel('City', { exact: true }).fill('Test City');
  await page.getByLabel('Postal code', { exact: true }).fill('10001');
  const preview = page.waitForResponse(r => r.url() === `${api}/preview` && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Review totals', exact: true }).click();
  expect((await preview).status()).toBe(200);
  await expect(page.getByRole('main')).toContainText('$31.40');
  const started = page.waitForResponse(r => r.url() === `${api}/checkout` && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Start test checkout', exact: true }).click();
  const response = await started;
  expect(response.status()).toBe(201);
  const checkout = await response.json();
  expect(checkout).toMatchObject({ state: 'ready', amount_minor: '3140', currency: 'USD' });
  await expect(page.getByRole('button', { name: 'Fake payment: capture', exact: true })).toBeEnabled();
  await expect(page.getByRole('link', { name: 'View order confirmation', exact: true })).toHaveCount(0);
  return checkout;
}

test('published guest purchase persists captured order', async ({ page, request }, info) => {
  const site = await publishedSite(request);
  expect(site.release_id).not.toBe('c0900005-0000-4000-8000-000000000001');
  const checkout = await start(page);
  await audit(page, info);
  const paid = page.waitForResponse(r => r.url() === `${api}/payment` && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Fake payment: capture', exact: true }).click();
  expect((await paid).status()).toBe(200);
  expect(await (await paid).json()).toMatchObject({ checkout_id: checkout.checkout_id,
    order_id: checkout.order_id, state: 'paid', payment: { status: 'captured' } });
  await page.getByRole('link', { name: 'View order confirmation', exact: true }).click();
  await expect(page.locator('[data-completion-ready]')).toBeVisible();
  await expect(page.getByRole('main')).toContainText('$31.40');
  // Fresh read from the BFF traverses the real authenticated Get path again.
  const persisted = await page.request.get(`${api}/completion`);
  expect(persisted.status()).toBe(200);
  expect(await persisted.json()).toMatchObject({ checkout_id: checkout.checkout_id,
    order_id: checkout.order_id, state: 'paid', payment: { status: 'captured' } });
  await info.attach('purchase-checkout', { body: JSON.stringify(await persisted.json()), contentType: 'application/json' });
  await audit(page, info);
});

test('pending receipt alone never shows paid', async ({ page, request }, info) => {
  await publishedSite(request);
  const checkout = await start(page);
  const key = process.env.OPENSITE_FAKE_WEBHOOK_KEY;
  if (!key) throw new Error('Missing isolated CI signing key');
  const event = { event_id: `c09d:${info.project.name}:refund-before-capture`, account_id: 'c09d-e2e',
    order_id: checkout.order_id, order_version: Number(checkout.order_version),
    provider_ref: checkout.payment.provider_ref, status: 'refunded', amount_minor: 3140, currency: 'USD' };
  const raw = JSON.stringify(event), timestamp = String(Math.floor(Date.now() / 1000));
  const signature = `t=${timestamp},v1=${createHmac('sha256', Buffer.from(key, 'base64url')).update(`${timestamp}.${raw}`).digest('hex')}`;
  const receipt = await request.post(`${fixture.api}/api/v1/testing/payments/fake/webhook`, {
    data: raw, headers: { 'Content-Type': 'application/json', 'X-Fake-Signature': signature }, maxRedirects: 0,
  });
  expect(receipt.status()).toBe(200);
  expect(await receipt.json()).toEqual({ event_id: event.event_id, status: 'pending' });
  await page.goto(`${fixture.storefront}/order-complete`);
  await expect(page.locator('[data-completion-page]')).toHaveAttribute('aria-busy', 'false');
  await expect(page.locator('[data-completion-ready]')).toHaveCount(0);
  const persisted = await page.request.get(`${api}/completion`);
  expect(persisted.status()).toBe(200);
  expect(await persisted.json()).toMatchObject({ checkout_id: checkout.checkout_id,
    state: 'ready', payment: { status: 'authorized' } });
  await audit(page, info);
});
