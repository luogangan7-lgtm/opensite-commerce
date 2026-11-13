/** Browser contract against static output, only synthetic intercepted RFQs. */
import { test, expect } from '@playwright/test';
import products from '../src/data/products.json' with { type: 'json' };
import identities from './card-identities.json' with { type: 'json' };
const cardTitle = (locale: string, sku: string) => identities.find(identity => identity.slug === sku)!.localized_titles[locale as keyof typeof identities[0]['localized_titles']];
const slug = products[0].slug;
test('contact prefill and language switching retain path/query/fragment', async ({ page }) => {
  await page.goto(`/contact/?product=${slug}&type=sample&subject=fabric-photos#rfq-form`);
  await expect(page.locator('textarea')).toHaveValue(`${slug} (${products[0].style_codes.join(' / ')})\nsample\nfabric-photos`);
  await page.locator('[data-menu-toggle]').click();
  await page.locator('.mobile-languages summary').click();
  await page.locator('.mobile-languages [data-locale-link][lang="zh"]').click();
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

 test('Chinese product facts are localized; L1 editorial pages are fully localized', async ({ page }) => {
  await page.goto(`/zh/products/${slug}/`);
  await expect(page.locator('[data-english-fallback]')).toHaveCount(0);
  await expect(page.locator('h1')).toContainText('卫衣');
  await page.goto('/zh/collections/');
  await expect(page.locator('.tile h3').first()).toContainText('卫衣');
  for (const locale of ['zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar']) {
    for (const route of ['shipping', 'privacy', 'terms']) {
      await page.goto(`/${locale}/${route}/`);
      await expect(page.locator('[data-english-fallback]')).toHaveCount(0);
      await expect(page.locator('h1')).not.toHaveAttribute('lang', 'en');
    }
  }
});

 test('Chinese digital alt and disclosure follow gallery selection', async ({ page }) => {
  // Catalog order is not a promise that the first style has a reviewed side view.
  const modelProduct = products.find(product => product.images.some(image => image.src.endsWith('/digital-turn.webp')));
  expect(modelProduct, 'requires a catalog style with an approved W3 turn view').toBeDefined();
  await page.goto(`/zh/products/${modelProduct!.slug}/`);
  await expect(page.locator('[data-gallery-main]')).toHaveAttribute('alt', /数字模特示意/);
  const turn = page.locator('.gallery-thumbs a[href$="digital-turn.webp"]');
  await turn.click();
  await expect(page.locator('[data-gallery-main]')).toHaveAttribute('alt', /侧转/);
  await expect(page.locator('[data-digital-label]')).toHaveText('数字模特示意');
  await page.locator('.gallery-thumbs a').last().click();
  await expect(page.locator('[data-digital-label]')).toBeEmpty();
  await page.goto('/zh/collections/');
  await expect(page.locator('.tile img').first()).toHaveAttribute('alt', /数字模特示意/);
});

// §14 per-SKU acceptance; fixed contract identities, existing locale dictionaries and suite.
// C3: card identity only; inquiry behavior belongs to the separate C1 task.
for (const locale of ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar'] as const) {
  const prefix = locale === 'en' ? '' : `/${locale}`;
  for (const width of [390, 768, 1440]) {
    for (const [sku, style] of [
      ['apparel-59043', '59043'], ['cotton430-hoodie-58065', '58065'],
      ['apparel-70171', '70171'], ['apparel-70175', '70175'],
      ['crew-fleece-8900-38', '8900-38'],
    ]) test(`card identity ${sku} ${locale} ${width}`, async ({ page }) => {
      await page.setViewportSize({ width, height: 1000 });
      // Fail closed: every RFQ is intercepted before navigation; no real customer request.
      await page.route('**/api/rfq', route => route.fulfill({ status: 503, contentType: 'application/json', body: '{}' }));
      await page.goto(`${prefix}/collections/`);
      const card = page.locator('.tile').filter({ has: page.locator('bdi', { hasText: new RegExp(`^${style}$`) }) });
      await expect(card).toHaveCount(1);
      const title = cardTitle(locale, sku);
      await expect(card.locator('h3')).toHaveText(title);
      await expect(card).toHaveAttribute('href', `${prefix}/products/${sku}/`);
      await card.click();
      await expect(page).toHaveURL(`${prefix}/products/${sku}/`);
      await expect(page.locator('h1')).toHaveText(title);
      await expect(page.locator('.product-info > .muted bdi')).toHaveText(style);
      await expect(page.locator('html')).toHaveAttribute('lang', locale);
      await expect(page.locator('html')).toHaveAttribute('dir', locale === 'ar' ? 'rtl' : 'ltr');
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);

    });
    test(`Home and related card entrances ${locale} ${width}`, async ({ page }) => {
      await page.setViewportSize({ width, height: 1000 });
      await page.route('**/api/rfq', route => route.abort());
      for (const [origin, sku] of [
        ['/', 'apparel-59043'], ['/', 'cotton430-hoodie-58065'],
        ['/', 'apparel-70171'], ['/', 'crew-fleece-8900-38'],
        ['/products/apparel-59043/', 'apparel-70171'],
      ]) {
        await page.goto(`${prefix}${origin}`);
        const card = page.locator(`.tile[href="${prefix}/products/${sku}/"]`);
        await expect(card).toHaveCount(1);
        await expect(card.locator('h3')).toHaveText(cardTitle(locale, sku));
        await card.click();
        await expect(page).toHaveURL(`${prefix}/products/${sku}/`);
        await expect(page.locator('h1')).toHaveText(cardTitle(locale, sku));
      }
    });
  }
}
