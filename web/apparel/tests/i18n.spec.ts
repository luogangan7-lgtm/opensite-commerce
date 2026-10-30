/** All Arabic routes at 390px plus reciprocal sampled hreflang and localized form errors. CI only. */
import { test, expect } from '@playwright/test';
import products from '../src/data/products.json';
const locales = ['en', 'es', 'fr', 'de', 'pt', 'ru', 'ar'];
const routes = ['/', '/collections/', '/about/', '/contact/', ...Array.from(new Set(products.map(p => `/collections/${p.category}/`))), ...products.map(p => `/products/${p.slug}/`)];
for (const route of routes) test(`Arabic 390px ${route}`, async ({ page }) => {
  expect((await page.goto(`/ar${route}`))?.status()).toBe(200);
  await expect(page.locator('html')).toHaveAttribute('lang', 'ar');
  await expect(page.locator('html')).toHaveAttribute('dir', 'rtl');
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  if (['/', '/collections/crew-fleece/', '/products/crew-fleece-8900-32/'].includes(route)) await page.screenshot({ path: `evidence/ar${route.replaceAll('/', '_')}.png`, fullPage: true });
});
for (const locale of locales) for (const route of ['/', '/collections/crew-fleece/', '/products/crew-fleece-8900-32/']) test(`hreflang ${locale} ${route}`, async ({ page }) => {
  await page.goto(`${locale === 'en' ? '' : `/${locale}`}${route}`);
  const links = page.locator('link[rel="alternate"][hreflang]');
  await expect(links).toHaveCount(8);
  for (const target of locales) {
    const path = `${target === 'en' ? '' : `/${target}`}${route}`;
    expect(new URL((await page.locator(`link[rel="alternate"][hreflang="${target}"]`).getAttribute('href'))!).pathname).toBe(path);
    await expect(page.locator(`.language-switcher a[lang="${target}"]`)).toHaveAttribute('href', path);
  }
  expect(new URL((await page.locator('link[hreflang="x-default"]').getAttribute('href'))!).pathname).toBe('/');
});
test('Spanish form renders server message keys as Spanish strings', async ({ page }) => {
  await page.route('**/api/rfq', route => route.fulfill({ status: 400, contentType: 'application/json', body: JSON.stringify({ errors: { email: 'error.email' } }) }));
  await page.goto('/es/contact/');
  await page.locator('[name="name"]').fill('CI'); await page.locator('[name="company"]').fill('Example');
  await page.locator('[name="email"]').fill('ci@example.invalid'); await page.locator('[name="message"]').fill('Sample');
  await page.locator('button[type="submit"]').click();
  await expect(page.locator('.form-status')).toContainText('Introduce una dirección de correo válida.');
});
