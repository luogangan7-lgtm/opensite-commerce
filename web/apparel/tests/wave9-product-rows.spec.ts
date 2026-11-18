/** Measure each rendered sibling row in the four catalog surfaces. */
import { expect, test } from '@playwright/test';

if (process.env.CP6_BROWSER_EXECUTABLE) test.use({ launchOptions: { executablePath: process.env.CP6_BROWSER_EXECUTABLE } });

const surfaces = [
  ['all-styles', '/collections/', '.shop-grid'],
  ['category', '/collections/cotton430-hoodie/', '.shop-grid'],
  ['home-popular', '/', '.home-card-grid'],
  ['related-styles', '/products/cotton430-hoodie-58065/', '.product-grid'],
] as const;

for (const locale of ['en', 'ar', 'zh']) for (const width of [390, 768, 1440]) {
  for (const [name, path, grid] of surfaces) test(`${name} ${locale} ${width}`, async ({ page }, info) => {
    await page.setViewportSize({ width, height: 900 });
    const prefix = locale === 'en' ? '' : `/${locale}`;
    await page.goto(prefix + path);
    await page.evaluate(() => document.fonts.ready);
    const rows = await page.locator(`${grid} > :visible`).evaluateAll(cards => {
      const rows = new Map<number, number[][]>();
      for (const card of cards) {
        const top = Math.round(card.getBoundingClientRect().top);
        const sibling = ['.shop-photo, img', '.shop-card-body, h3, strong', '.shop-quote'].flatMap(selector => {
          const box = card.querySelector(selector)?.getBoundingClientRect();
          return box ? [box.top, box.bottom, box.width] : [];
        });
        const row = rows.get(top) ?? [];
        row.push(sibling);
        rows.set(top, row);
      }
      return [...rows.values()];
    });
    await info.attach('row-boxes', { body: JSON.stringify(rows), contentType: 'application/json' });
    expect(rows[0]?.length, 'first-row columns').toBe(width === 1440 ? 4 : 2);
    for (const [index, row] of rows.entries()) {
      for (const positions of row) expect(positions, `row ${index + 1} needs photo, body and CTA`).toHaveLength(9);
      // Width matters for the photos; text and CTA widths follow their localized copy.
      for (const coordinate of [0, 1, 2, 3, 4, 6, 7]) {
        const values = row.map(positions => positions[coordinate]);
        expect(Math.max(...values) - Math.min(...values), `row ${index + 1}, coordinate ${coordinate}`).toBeLessThanOrEqual(2);
      }
    }
  });
}
