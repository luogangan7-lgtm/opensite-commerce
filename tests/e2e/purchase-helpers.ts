/** Real storefront purchase setup shared by purchase and RMA CI journeys.
 * Depends on the real BFF and published fixture; no interception or SQL writes.
 */
import { expect, type Page } from '@playwright/test';
import { fixture } from './fixture';
const api = `${fixture.storefront}/purchase/api`;
export async function start(page: Page, customer = false, quantity = 1) {
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
  if (quantity === 2) {
    await page.getByLabel('Quantity', { exact: true }).fill('2');
    await page.getByRole('button', { name: 'Update quantity', exact: true }).click();
    await expect(page.getByRole('main')).toContainText('$48.00');
  }
  const total = quantity === 2 ? '5780' : '3140';
  await page.goto(`${fixture.storefront}/checkout`);
  if (!customer) await page.getByLabel('Email', { exact: true }).fill('c09d-buyer@example.test');
  await page.getByLabel('Country code', { exact: true }).fill('US');
  await page.getByLabel('Address line 1', { exact: true }).fill('1 Test Way');
  await page.getByLabel('City', { exact: true }).fill('Test City');
  await page.getByLabel('Postal code', { exact: true }).fill('10001');
  const preview = page.waitForResponse(r => r.url() === `${api}/preview` && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Review totals', exact: true }).click();
  expect((await preview).status()).toBe(200);
  await expect(page.getByRole('main')).toContainText(quantity === 2 ? '$57.80' : '$31.40');
  const started = page.waitForResponse(r => r.url() === `${api}/checkout` && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Start test checkout', exact: true }).click();
  const response = await started;
  expect(response.status()).toBe(201);
  const checkout = await response.json();
  expect(checkout).toMatchObject({ state: 'ready', amount_minor: total, currency: 'USD' });
  await expect(page.getByRole('button', { name: 'Fake payment: capture', exact: true })).toBeEnabled();
  await expect(page.getByRole('link', { name: 'View order confirmation', exact: true })).toHaveCount(0);
  return checkout;
}

