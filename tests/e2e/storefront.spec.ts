/** Responsibility: prove the real published-read API reaches storefront SSR and browser navigation.
 * Dependencies: disposable PostgreSQL release seed, API and standalone Astro; caller: CI E2E.
 * Invariants: visible loaded content and exact sealed release, no mocks or unpublished/foreign data.
 */
import { expect, test } from '@playwright/test';
import { assertPublicOnly, audit, fixture, publishedSite } from './fixture';

const pages = [
  { name: 'home', path: '/', title: 'Made for everyday use',
    description: 'A published example of pages and products.',
    text: 'Browse our published collection and learn how this sample site is made.' },
  { name: 'products', path: '/products', title: 'Products',
    description: 'A published example of pages and products.', text: fixture.productName },
  { name: 'product detail', path: `/products/${fixture.productSlug}`, title: fixture.productName,
    description: 'A reusable cotton tote for daily essentials.', text: '$24.00' },
  { name: 'CMS', path: `/${fixture.cmsSlug}`, title: 'About this shop',
    description: 'About this published shop.', text: 'This page belongs to the published release.' },
] as const;

for (const item of pages) {
  test(`published storefront ${item.name} loads actual sealed release`, async ({ page, request }, info) => {
    const site = await publishedSite(request);
    await info.attach('published-read-receipt', { body: JSON.stringify(site), contentType: 'application/json' });
    const url = `${fixture.storefront}${item.path}`;
    // Independent HTTP receipt must contain content before JavaScript hydration.
    const html = await request.get(url, { maxRedirects: 0 });
    expect(html.status()).toBe(200);
    expect(html.headers()['cache-control']).toContain('no-store');
    const source = await html.text();
    expect(source).toContain(item.title);
    expect(source).toContain(fixture.releaseId);
    assertPublicOnly(source);
    const response = await page.goto(url);
    expect(response?.status()).toBe(200);
    await expect(page.locator('[data-storefront-ready="true"]')).toBeVisible();
    await expect(page.locator('[data-storefront-ready="true"]')).toHaveAttribute('data-release-id', site.release_id);
    await expect(page.getByRole('heading', { level: 1 })).toHaveText(item.title);
    await expect(page.getByRole('main')).toContainText(item.text);
    await expect(page).toHaveTitle(`${item.title} | ${site.title}`);
    await expect(page.locator('meta[name="description"]')).toHaveAttribute('content', item.description);
    await expect(page.locator('link[rel="canonical"]')).toHaveAttribute('href', url);
    assertPublicOnly(await page.locator('body').innerText());
    await audit(page, info);
  });
}

test('published navigation and keyboard skip follow real CMS/product routes', async ({ page, request }, info) => {
  await publishedSite(request);
  expect((await page.goto(fixture.storefront))?.status()).toBe(200);
  await expect(page.locator('[data-storefront-ready="true"]')).toBeVisible();
  await page.keyboard.press('Tab');
  await expect(page.getByRole('link', { name: 'Skip to content' })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('main')).toBeFocused();
  await audit(page, info);
  await page.getByRole('navigation').getByRole('link', { name: 'About', exact: true }).click();
  await expect(page).toHaveURL(`${fixture.storefront}/${fixture.cmsSlug}`);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('About this shop');
  await audit(page, info);
  await page.getByRole('navigation').getByRole('link', { name: 'Products', exact: true }).click();
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Products');
  await audit(page, info);
  await page.getByRole('link', { name: fixture.productName, exact: true }).click();
  await expect(page).toHaveURL(`${fixture.storefront}/products/${fixture.productSlug}`);
  await expect(page.getByRole('main')).toContainText('$24.00');
  await audit(page, info);
  await page.getByRole('link', { name: 'Back to products' }).click();
  await expect(page).toHaveURL(`${fixture.storefront}/products`);
});

test('real guest browse → cart quantity → checkout → Fake capture → order receipt', async ({ page, request, context }, info) => {
  const site = await publishedSite(request);
  const skuId = 'c2400008-0000-4000-8000-000000000001';
  expect(site.products.find(product => product.id === fixture.productId)).toMatchObject({
    skus: [{ sku_id: skuId, available: true, price_minor: '2400', currency: 'USD' }],
  });
  const purchaseCalls: string[] = [];
  page.on('request', outgoing => {
    if (outgoing.url().includes('/purchase/api/')) purchaseCalls.push(outgoing.url());
  });

  await page.goto(`${fixture.storefront}/products`);
  await page.getByRole('link', { name: fixture.productName, exact: true }).click();
  await expect(page).toHaveURL(`${fixture.storefront}/products/${fixture.productSlug}`);
  await expect(page.locator('[data-purchase-ready="true"]')).toBeVisible();
  const purchaseContext = await page.request.get(`${fixture.storefront}/purchase/api/context`);
  expect(purchaseContext.status()).toBe(200);
  expect(await purchaseContext.json()).toMatchObject({ enabled: true, fixture: false, actor: 'guest' });
  await expect(page.getByRole('button', { name: 'Add to cart', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Add to cart', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('Item added to cart.');
  await page.getByRole('link', { name: 'View cart', exact: true }).click();
  await expect(page).toHaveURL(`${fixture.storefront}/cart`);
  await expect(page.locator(`[data-cart-line="${skuId}"]`)).toBeVisible();
  await page.getByLabel('Quantity', { exact: true }).fill('2');
  await page.getByRole('button', { name: 'Update quantity', exact: true }).click();
  await expect(page.getByRole('main')).toContainText('$48.00');
  await audit(page, info);

  await page.getByRole('link', { name: 'Continue to checkout', exact: true }).click();
  await expect(page).toHaveURL(`${fixture.storefront}/checkout`);
  await page.getByLabel('Email', { exact: true }).fill('c24-buyer@example.test');
  await page.getByLabel('Country code', { exact: true }).fill('US');
  await page.getByLabel('Address line 1', { exact: true }).fill('1 Test Way');
  await page.getByLabel('City', { exact: true }).fill('Test City');
  await page.getByLabel('Postal code', { exact: true }).fill('10001');
  await page.getByRole('button', { name: 'Review totals', exact: true }).click();
  await expect(page.getByRole('main')).toContainText('$57.80');
  await audit(page, info);

  const checkoutResponse = page.waitForResponse(response => response.url() === `${fixture.storefront}/purchase/api/checkout` && response.request().method() === 'POST');
  await page.getByRole('button', { name: 'Start test checkout', exact: true }).click();
  const started = await checkoutResponse;
  expect(started.status()).toBe(201);
  const checkout = await started.json() as { checkout_id: string; order_id: string; payment: { status: string } };
  expect(checkout.checkout_id).toMatch(/^[0-9a-f-]{36}$/i);
  expect(checkout.order_id).toMatch(/^[0-9a-f-]{36}$/i);
  expect(checkout.payment.status).not.toBe('captured');
  await expect(page.getByRole('link', { name: 'View order confirmation', exact: true })).toHaveCount(0);
  const paymentResponse = page.waitForResponse(response => response.url() === `${fixture.storefront}/purchase/api/payment` && response.request().method() === 'POST');
  await page.getByRole('button', { name: 'Fake payment: capture', exact: true }).click();
  const payment = await paymentResponse;
  expect(payment.status()).toBe(200);
  expect(await payment.json()).toMatchObject({ order_id: checkout.order_id, payment: { status: 'captured' } });
  await page.getByRole('link', { name: 'View order confirmation', exact: true }).click();
  await expect(page).toHaveURL(`${fixture.storefront}/order-complete`);
  await expect(page.locator('[data-completion-ready="true"][data-payment-status="captured"]')).toBeVisible();
  await expect(page.getByRole('main')).toContainText(checkout.order_id);
  await expect(page.getByRole('main')).toContainText('$57.80');
  await audit(page, info);

  const session = (await context.cookies(fixture.storefront)).find(cookie => cookie.name === '__Host-osc_storefront_fixture');
  expect(session).toMatchObject({ httpOnly: true, secure: true, sameSite: 'Lax', path: '/' });
  const browserStorage = await page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage }, cookie: document.cookie }));
  for (const exposed of [browserStorage, page.url(), await page.content(), ...purchaseCalls]) {
    expect(exposed).not.toContain(session!.value);
    expect(exposed).not.toMatch(/cart_token|X-Cart-Token|X-Fixture-Session/i);
    expect(exposed).not.toContain('127.0.0.1:4176');
  }
});

test('missing and unpublished CMS/product routes render accessible real 404', async ({ page, request }, info) => {
  await publishedSite(request);
  for (const path of ['/c24-missing-page', '/products/c24-missing-product', '/products/unpublished-sample']) {
    const response = await page.goto(`${fixture.storefront}${path}`);
    expect(response?.status()).toBe(404);
    expect(response?.headers()['cache-control']).toContain('no-store');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText('Page not found');
    await expect(page.locator('[data-storefront-ready]')).toHaveCount(0);
    await expect(page.locator('meta[name="robots"]')).toHaveAttribute('content', 'noindex');
    assertPublicOnly(await page.content());
    await audit(page, info);
  }
});

test('public host resolution denies pending disabled unpublished and unknown domains', async ({ request }) => {
  await publishedSite(request);
  for (const host of ['pending.c09.test', 'disabled.c09.test', 'unpublished.c09.test', 'unknown.c24.test']) {
    const response = await request.get(`${fixture.api}/api/v1/public/sites/current`, {
      params: { host }, maxRedirects: 0,
    });
    expect(response.status()).toBe(404);
    expect(response.headers()['content-type']).toContain('application/problem+json');
    const body = await response.text();
    expect(JSON.parse(body)).toMatchObject({ status: 404 });
    expect(body).not.toContain(fixture.releaseId);
    expect(body).not.toContain(fixture.productName);
    assertPublicOnly(body);
  }
});
