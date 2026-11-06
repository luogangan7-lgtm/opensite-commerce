/** Browser contract against static output, only synthetic intercepted RFQs. */
import { test, expect } from '@playwright/test';
import products from '../src/data/products.json' with { type: 'json' };
const slug = products[0].slug;
test('contact prefill and language switching retain path/query/fragment', async ({ page }) => {
  await page.goto(`/contact/?product=${slug}&type=sample&subject=fabric-photos#rfq-form`);
  await expect(page.locator('textarea')).toHaveValue(`${slug} (${products[0].style_codes.join(' / ')})\nsample\nfabric-photos`);
  await page.locator('.language-switcher summary').click();
  await page.locator('[data-locale-link][lang="zh"]').click();
  expect(new URL(page.url()).pathname).toBe('/zh/contact/');
  expect(new URL(page.url()).searchParams.get('product')).toBe(slug);
  expect(new URL(page.url()).hash).toBe('#rfq-form');
  await expect(page.locator('[name="locale"]')).toHaveValue('zh');
  await expect(page.locator('textarea')).toHaveValue(`${slug} (${products[0].style_codes.join(' / ')})\nsample\nfabric-photos`);
});
test('invalid query and obsolete fragment query are ignored; sent status renders', async ({ page }) => {
  await page.goto('/contact/?product=unknown&type=bad&subject=%3Cscript%3E#rfq-form?subject=fabric-photos');
  await expect(page.locator('textarea')).toHaveValue('');
  await page.goto('/zh/contact/?sent=1');
  await expect(page.locator('.form-status')).toHaveText(/已收到/);
});
async function fill(page: import('@playwright/test').Page) {
  await page.locator('[name="name"]').fill('CI');
  await page.locator('[name="company"]').fill('Example');
  await page.locator('[name="email"]').fill('ci@example.invalid');
  await page.locator('[name="message"]').fill('Synthetic sample request');
}
test('failed submission retains values, retry succeeds at localized sent route', async ({ page }) => {
  let calls = 0;
  await page.route('**/api/rfq', route => route.fulfill({ status: ++calls === 1 ? 503 : 201, contentType: 'application/json', body: '{}' }));
  await page.goto('/zh/contact/'); await fill(page);
  await page.locator('button[type="submit"]').click();
  await expect(page.locator('.form-status')).toHaveText(/未保存/);
  await expect(page.locator('textarea')).toHaveValue('Synthetic sample request');
  await expect(page.locator('.form-status')).toBeFocused();
  await page.locator('button[type="submit"]').click();
  await expect(page).toHaveURL(/\/zh\/contact\/\?sent=1$/);
});
test('15s timeout disables duplicates and allows retry without clearing fields', async ({ page }) => {
  await page.clock.install();
  await page.route('**/api/rfq', async () => {});
  await page.goto('/contact/'); await fill(page);
  const button = page.locator('button[type="submit"]');
  await button.click(); await expect(button).toBeDisabled();
  await page.clock.fastForward(15001);
  await expect(page.locator('.form-status')).toHaveText(/not sent/);
  await expect(button).toBeEnabled();
  await expect(page.locator('textarea')).toHaveValue('Synthetic sample request');
});

for (const locale of ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar']) {
  const prefix = locale === 'en' ? '' : `/${locale}`;
  for (const route of [`products/${slug}`, 'shipping']) {
    test(`non-contact success ${locale} ${route}`, async ({ page }) => {
      await page.route('**/api/rfq', route => route.fulfill({ status: 201, contentType: 'application/json', body: '{}' }));
      await page.goto(`${prefix}/${route}/`);
      await fill(page);
      await page.locator('button[type="submit"]').click();
      await expect(page).toHaveURL(`${prefix}/contact/?sent=1`);
      await expect(page.locator('.form-status')).not.toBeEmpty();
    });
  }
}

 test('English fallback is disclosed and language tagged', async ({ page }) => {
  await page.goto(`/zh/products/${slug}/`);
  await expect(page.locator('[data-english-fallback]').first()).toContainText('英文');
  await expect(page.locator('h1').locator('..')).toHaveAttribute('lang', 'en');
  await page.goto('/zh/collections/');
  await expect(page.locator('.tile h3').first()).toHaveAttribute('lang', 'en');
  for (const locale of ['zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar']) {
    for (const route of ['shipping', 'privacy', 'terms']) {
      await page.goto(`/${locale}/${route}/`);
      await expect(page.locator('[data-english-fallback]')).toBeVisible();
      await expect(page.locator('h1')).toHaveAttribute('lang', 'en');
    }
  }
});
