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
    description: 'A reusable cotton tote for daily essentials.', text: 'Add to cart' },
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
    expect(source).toContain(site.release_id);
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
  await expect(page.getByRole('main')).toContainText('Add to cart');
  await audit(page, info);
  await page.getByRole('link', { name: 'Back to products' }).click();
  await expect(page).toHaveURL(`${fixture.storefront}/products`);
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
