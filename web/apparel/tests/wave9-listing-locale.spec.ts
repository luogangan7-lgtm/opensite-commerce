/** Catalog UI stays localized across the store, Home and PDP surfaces. */
import { expect, test } from '@playwright/test';
import { readFileSync } from 'node:fs';
import type { Product } from '../src/data/catalog';
const products: Product[] = JSON.parse(readFileSync(new URL('../src/data/products.json', import.meta.url), 'utf8'));

if (process.env.CP6_BROWSER_EXECUTABLE) test.use({ launchOptions: { executablePath: process.env.CP6_BROWSER_EXECUTABLE } });

const allowed = new Set([
  'MOQ', 'EXW', 'FOB', 'OEM', 'ODM', 'Logo', 'GSM', 'Pantone', 'DTG', 'DTF',
  'WhatsApp', 'Cookie', 'GA4', 'Google', 'user-agent', 'Consent', 'Mode',
  'kg', 'cm', 'mm', 'g', 'pcs', 'sourcyowear.com',
  ...products.flatMap(product => product.style_codes),
].map(value => value.toLowerCase()));

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
    const words = copy.match(/[\p{Script=Latin}][\p{Script=Latin}\d.-]*/gu) ?? [];
    expect([...new Set(words.filter(word => !allowed.has(word.toLowerCase())))], path).toEqual([]);
  }
});
