/** Catalog UI stays localized across the store, Home and PDP surfaces. */
import { expect, test } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { ownerMoq } from '../src/data/owner-facts';
import type { Product } from '../src/data/catalog';
const products: Product[] = JSON.parse(readFileSync(new URL('../src/data/products.json', import.meta.url), 'utf8'));

if (process.env.CP6_BROWSER_EXECUTABLE) test.use({ launchOptions: { executablePath: process.env.CP6_BROWSER_EXECUTABLE } });

const allowed = new Set([
  'MOQ', 'EXW', 'FOB', 'OEM', 'ODM', 'Logo', 'GSM', 'Pantone', 'DTG', 'DTF',
  'WhatsApp', 'Cookie', 'GA4', 'Google', 'user-agent', 'Consent', 'Mode',
  'kg', 'cm', 'mm', 'g', 'pcs', 'sourcyowear.com',
  ...products.flatMap(product => product.style_codes),
].map(value => value.toLowerCase()));

test('F3 Chinese catalog banner keeps the samples and trade terms together', async ({ page }) => {
  const custom = ownerMoq.custom.toLocaleString('en-US');
  for (const path of ['/zh/collections/', '/zh/collections/cotton430-hoodie/']) {
    await page.setViewportSize({ width: 390, height: 900 });
    await page.goto(path);
    const banner = page.locator('.shop-b2b');
    await expect(banner.getByRole('link', { name: '获取报价', exact: true })).toHaveText('获取报价 →');
    await expect(banner.locator('.shop-terms')).toHaveText(`${ownerMoq.ready} 件起订 / 定制 ${custom} 件起 / 样品 / EXW 或 FOB`);
    const samples = banner.getByRole('link', { name: '样品', exact: true });
    await expect(samples).toHaveAttribute('href', '/zh/sampling/');
    const trade = banner.getByText('EXW 或 FOB', { exact: true });
    await page.evaluate(() => document.fonts.ready);
    for (const width of [390, 320]) {
      await page.setViewportSize({ width, height: 900 });
      const lines = await banner.locator('.shop-term').evaluateAll(facts =>
        new Set(facts.map(fact => Math.round(fact.getBoundingClientRect().top))).size);
      expect(lines, `${path} at ${width}px`).toBe(width === 390 ? 1 : 2);
      const terms = await trade.boundingBox();
      const link = await samples.boundingBox();
      expect(terms!.y, `${path} at ${width}px`).toBeCloseTo(link!.y, 0);
      expect(terms!.x + terms!.width).toBeLessThanOrEqual(width);
    }
  }
});

for (const locale of ['ar', 'zh']) test(`localized catalog copy ${locale}`, async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  for (const [path, selector] of [
    ['/collections/', '.shop'], ['/collections/cotton430-hoodie/', '.shop'],
    ['/', '.home-products'], ['/products/cotton430-hoodie-58065/', '.product-grid'],
  ]) {
    await page.goto(`/${locale}${path}`);
    const scope = page.locator(selector);
    const labels = await scope.locator('.product-card-title').allTextContents();
    expect(labels.length).toBeGreaterThan(0);
    // All styles renders the rest of the catalog for progressive enhancement and pagination.
    if (path === '/collections/') expect(labels).toHaveLength(products.length);
    const copy = [await scope.innerText(), ...labels].join(' ');
    // The approved Chinese T-shirt label contains a Latin T in this exact phrase.
    const prose = locale === 'zh' ? copy.replace(/(?<!\p{Script=Latin})T[ \u00a0]恤/gu, '') : copy;
    const words = prose.match(/[\p{Script=Latin}][\p{Script=Latin}\d.-]*/gu) ?? [];
    expect([...new Set(words.filter(word => !allowed.has(word.toLowerCase())))], path).toEqual([]);
  }
});

const f3Copy = {
  en: ['Oversized T-shirt', 'Fleece hoodie', 'Style', '4 colours', `MOQ ${ownerMoq.ready} pcs`, 'Request a quote', 'Popular styles', 'Related styles', 'T-shirts', 'Category'],
  ar: ['تيشيرت واسع', 'هودي فليس', 'التصميم', '4 ألوان', `الحد الأدنى ${ownerMoq.ready} قطعة`, 'اطلب عرض سعر', 'موديلات رائجة', 'موديلات ذات صلة', 'تيشيرتات', 'الفئة'],
  zh: ['宽松 T 恤', '连帽卫衣', '款号', '4 种颜色', `起订量 ${ownerMoq.ready} 件`, '获取报价', '热门款式', '相关款式', 'T 恤', '类目'],
} as const;

// Literal wording comes from the approved F3 frames; sample colour totals come from the catalog.
for (const [locale, copy] of Object.entries(f3Copy)) test(`F3 catalog copy ${locale}`, async ({ page }) => {
  const [tee, fleece, style, colours, moq, quote, popular, related, tab, category] = copy;
  const prefix = locale === 'en' ? '' : `/${locale}`;
  await page.setViewportSize({ width: 390, height: 900 });
  for (const [path, slug, name, code] of [
    ['/collections/', 'apparel-59043', tee, '59043'],
    ['/collections/cotton430-hoodie/', 'cotton430-hoodie-58063', fleece, '58063'],
    ['/', 'cotton430-hoodie-58065', fleece, '58065'],
    ['/products/cotton430-hoodie-58065/', 'cotton430-hoodie-58063', fleece, '58063'],
  ]) {
    await page.goto(prefix + path);
    const card = page.locator(`[data-product-card="${slug}"]`);
    await expect(card.locator('.product-card-title')).toHaveText(name);
    await expect(card.locator('.product-card-code')).toHaveText(`${style} ${code}`);
    await expect(card.locator('.shop-colours')).toHaveText(colours);
    await expect(card.locator('.shop-moq')).toHaveText(moq);
    await expect(card.locator('.shop-quote')).toHaveText(quote);
    if (locale === 'ar') {
      await page.evaluate(() => document.fonts.ready);
      const lineCount = await card.locator('.shop-moq').evaluate(element => {
        const range = document.createRange();
        range.selectNodeContents(element);
        return new Set([...range.getClientRects()].map(box => Math.round(box.top))).size;
      });
      expect(lineCount, path).toBe(1);
    }
    if (path === '/') await expect(page.locator('.home-products h2')).toHaveText(popular);
    if (path.startsWith('/products/')) await expect(page.locator('.pdp-related h2')).toHaveText(related);
    if (path === '/collections/') {
      const apparelTab = page.locator(`.shop-tabs a[href="${prefix}/collections/apparel/"]`);
      expect(await apparelTab.textContent()).toBe(tab);
      await page.locator('[data-shop-filter-open]').click();
      await expect(page.locator('[data-shop-filter-dialog] fieldset').first().locator('legend')).toHaveText(category);
    }
  }
});
