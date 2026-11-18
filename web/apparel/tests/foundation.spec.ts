/** Browser contract against static output, only synthetic intercepted RFQs. */
import { test, expect } from '@playwright/test';
import {readFileSync} from 'node:fs';
import {shopNameKeys} from '../src/data/shop';
import products from '../src/data/products.json' with { type: 'json' };
import identities from './card-identities.json' with { type: 'json' };
import en from '../src/i18n/en.json' with { type: 'json' };
import zh from '../src/i18n/zh.json' with { type: 'json' };
import enProducts from '../src/i18n/products.en.json' with { type: 'json' };
import zhProducts from '../src/i18n/products.zh.json' with { type: 'json' };
const cardTitle = (locale: string, sku: string) => identities.find(identity => identity.slug === sku)!.localized_titles[locale as keyof typeof identities[0]['localized_titles']];
// The F3 PDP heading is shorter than the full product name carried on the card.
const f3HoodieTitles: Partial<Record<string,string>> = {
  en: 'Fleece pullover hoodie',
  ar: 'هودي ببطانة فليس',
  zh: '加绒套头帽衫',
};
const pdpTitle = (locale: string, sku: string) =>
  (sku === 'cotton430-hoodie-58065' && f3HoodieTitles[locale]) || cardTitle(locale, sku);
const listingTitle=(locale:string,sku:string)=>{const p=products.find(p=>p.slug===sku)!;const key=shopNameKeys[p.style_codes[0]];return key?JSON.parse(readFileSync(new URL(`../src/i18n/${locale}.json`,import.meta.url),'utf8'))[key]:locale==='zh'?cardTitle(locale,sku).replace(/T\s*恤/gu,'短袖上衣'):cardTitle(locale,sku);};
const slug = products[0].slug;
test('contact prefill and language switching retain path/query/fragment', async ({ page }) => {
  await page.goto(`/contact/?product=${slug}&type=sample&subject=fabric-photos#rfq-form`);
  await expect(page.locator('textarea')).toHaveValue(`${enProducts[slug].name} (${slug}; ${products[0].style_codes.join(' / ')})\n${en.requestSample}\nfabric-photos`);
  const mobile = await page.locator('[data-menu-toggle]').isVisible();
  if (mobile) await page.locator('[data-menu-toggle]').click();
  const switcher = page.locator(mobile ? '.mobile-languages' : '.language-switcher');
  await switcher.locator('summary').click();
  await switcher.locator('[data-locale-link][lang="zh"]').click();
  expect(new URL(page.url()).pathname).toBe('/zh/contact/');
  expect(new URL(page.url()).searchParams.get('product')).toBe(slug);
  expect(new URL(page.url()).searchParams.get('type')).toBe('sample');
  expect(new URL(page.url()).searchParams.get('subject')).toBe('fabric-photos');
  await expect(page.locator('[name="product_slug"]')).toHaveValue(slug);
  expect(new URL(page.url()).hash).toBe('#rfq-form');
  await expect(page.locator('[name="locale"]')).toHaveValue('zh');
  await expect(page.locator('textarea')).toHaveValue(`${zhProducts[slug].name} (${slug}; ${products[0].style_codes.join(' / ')})\n${zh.requestSample}\nfabric-photos`);
});
test('invalid query and obsolete fragment query are ignored; sent status renders', async ({ page }) => {
  await page.goto('/contact/?product=unknown&type=bad&subject=%3Cscript%3E#rfq-form?subject=fabric-photos');
  await expect(page.locator('textarea')).toHaveValue('');
  await page.goto('/zh/contact/?sent=1');
  await expect(page.locator('[data-rfq-success]')).toContainText(/已收到/);
});
async function fill(page: import('@playwright/test').Page) {
  await page.locator('[name="first_name"]').fill('CI'); await page.locator('[name="last_name"]').fill('Buyer'); await page.locator('[name="quantity_note"]').fill('100 pieces');
  await page.locator('[name="company"]').fill('Example');
  await page.locator('[name="email"]').fill('ci@example.invalid');
  await page.locator('[name="message"]').fill('Synthetic sample request');
}
test('failed submission retains values, retry succeeds at localized sent route', async ({ page }) => {
  let calls = 0;
  await page.route('**/api/rfq', route => route.fulfill({ status: ++calls === 1 ? 503 : 201, contentType: 'application/json', body: '{}' }));
  await page.goto('/zh/contact/'); await fill(page);
  await page.locator('button[type="submit"]').click();
  await expect(page.locator('.form-status')).toHaveText(zh.rfqServerError);
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
    test(`quote success from ${locale} ${route}`, async ({ page }) => {
      await page.route('**/api/rfq', route => route.fulfill({ status: 201, contentType: 'application/json', body: '{}' }));
      await page.goto(`${prefix}/${route}/`);
      if (route === 'shipping') await page.locator('[data-service-rfq]').click();
      else {
        await page.locator('[data-rfq-intent=quote]').click();
        await expect(page.locator('[name=product_slug]')).toHaveValue(slug);
      }
      await fill(page);
      await page.locator('button[type="submit"]').click();
      await expect(page).toHaveURL(`${prefix}/contact/?sent=1`);
      await expect(page.locator('[data-rfq-success]')).toBeVisible();
    });
  }
}

 test('Chinese product facts are localized; L1 editorial pages are fully localized', async ({ page }) => {
  await page.goto(`/zh/products/${slug}/`);
  await expect(page.locator('[data-english-fallback]')).toHaveCount(0);
  await expect(page.locator('h1')).toContainText('卫衣');
  await page.goto('/zh/collections/');
  await expect(page.locator('[data-shop-card] h2').first()).toHaveText(listingTitle('zh','apparel-59043'));
  for (const locale of ['zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar']) {
    for (const route of ['shipping', 'privacy', 'terms']) {
      await page.goto(`/${locale}/${route}/`);
      await expect(page.locator('[data-english-fallback]')).toHaveCount(0);
      await expect(page.locator('h1')).not.toHaveAttribute('lang', 'en');
    }
  }
});

 test('Chinese descriptive alt follows gallery selection without captions', async ({ page }) => {
  // Catalog order is not a promise that the first style has a reviewed side view.
  const modelProduct = products.find(product => product.images.some(image => image.src.endsWith('/digital-turn.webp')));
  expect(modelProduct, 'requires a catalog style with an approved W3 turn view').toBeDefined();
  await page.goto(`/zh/products/${modelProduct!.slug}/`);
  await expect(page.locator('[data-gallery-main]')).toHaveAttribute('alt', /正面/);
  await page.setViewportSize({width:1440,height:1000});
  const turn = page.locator('.gallery-thumbs a[href$="digital-turn.webp"]');
  await turn.click();
  await expect(page.locator('[data-gallery-main]')).toHaveAttribute('alt', /侧转/);
  await expect(page.locator('[data-digital-label]')).toHaveCount(0);
  await page.locator('.gallery-thumbs:not([hidden]) a[data-role="front"]').click();
  await expect(page.locator('[data-digital-label]')).toHaveCount(0);
  await page.goto('/zh/collections/');
  await expect(page.locator('[data-shop-card] .shop-photo img').first()).toHaveAttribute('alt', /正面/);
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
      const category = products.find(product => product.slug === sku)!.category;
      await page.goto(`${prefix}/collections/${category}/`);
      const card = page.locator('[data-shop-card]').filter({ has: page.locator('bdi', { hasText: new RegExp(`^${style}$`) }) });
      while(!await card.isVisible() && await page.locator('[data-shop-more]').isVisible()) await page.locator('[data-shop-more]').click();
      await expect(card).toHaveCount(1);
      await expect(card.locator('h2')).toHaveText(listingTitle(locale,sku));
      const entrance=card.locator('h2 a');
      await expect(entrance).toHaveAttribute('href', `${prefix}/products/${sku}/`);
      await entrance.click();
      await expect(page).toHaveURL(`${prefix}/products/${sku}/`);
      await expect(page.locator('h1')).toHaveText(pdpTitle(locale, sku));
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
        const tile = page.locator(`[data-product-card="${sku}"]`);
        await expect(tile).toHaveCount(1);
        const card = tile.locator('.product-card-title a');
        await expect(card).toHaveAttribute('href', `${prefix}/products/${sku}/`);
        await expect(card).toHaveAttribute('title', cardTitle(locale, sku));
        if (origin === '/') await expect(card).not.toBeEmpty();
        else await expect(card).toHaveText(listingTitle(locale, sku));
        await card.click();
        await expect(page).toHaveURL(`${prefix}/products/${sku}/`);
        await expect(page.locator('h1')).toHaveText(pdpTitle(locale, sku));
      }
    });
  }
}
