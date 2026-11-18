/** WAVE9 C4 rendered row geometry at the three F3 viewport widths. */
import { expect, test, type Locator } from '@playwright/test';

async function expectAligned(group: Locator, item: string, parts: string[]) {
  const rows = await group.locator(item).evaluateAll((elements, selectors) => {
    const rows = new Map<number, DOMRect[][]>();
    for (const element of elements) {
      const rect = element.getBoundingClientRect();
      const key = Math.round(rect.top);
      const row = rows.get(key) ?? [];
      row.push(selectors.map(selector => element.querySelector(selector)!.getBoundingClientRect()));
      rows.set(key, row);
    }
    return [...rows.values()].flatMap(row => row[0].flatMap((_, i) => {
      const rects = row.map(item => item[i]);
      return [rects.map(rect => rect.top), rects.map(rect => rect.bottom)];
    }));
  }, parts);
  expect(rows.length).toBeGreaterThan(0);
  for (const edges of rows) expect(Math.max(...edges) - Math.min(...edges)).toBeLessThanOrEqual(2);
}

for (const locale of ['en', 'ar', 'zh']) for (const width of [390, 768, 1440]) {
  test(`C4 sibling rows and shell · ${locale} ${width}`, async ({ page }) => {
    await page.setViewportSize({ width, height: 844 });
    const base = locale === 'en' ? '/' : `/${locale}/`;
    await page.goto(base + 'about-us/');
    await page.evaluate(() => document.fonts.ready);
    await expect(page.locator('main h2').first()).toHaveCSS('font-weight', '400');
    await expectAligned(page.locator('.about-catalog'), 'a', ['img', 'span']);
    await expectAligned(page.locator('.about-ways'), 'li', ['h3', 'p']);
    await expectAligned(page.locator('.about-terms'), '.about-copy', ['h3', 'p']);
    await expect(page.locator('.quote-float')).toHaveCount(0);
    if (width === 390) await expect(page.locator('[data-menu-toggle]').filter({ hasText: '≡' })).toBeVisible();
    if (width === 1440) {
      const cta = page.locator('header .button:visible');
      const switcher = page.locator('header .language-switcher');
      await expect(cta).toContainText(locale === 'ar' ? '←' : '→');
      await expect(switcher.locator('.nav-caret')).toBeVisible();
      expect((await switcher.boundingBox())!.x).toBeLessThan((await cta.boundingBox())!.x);
    }
    await expect(page.locator('footer nav a').first()).toHaveCSS('color', 'rgb(201, 194, 178)');
    await expect(page.locator('.footer-legal bdi')).toHaveText('© 2026 Sourcyo Apparel');
    if (locale === 'zh') await expect(page.locator('footer a[href$="/oem-odm/"]')).toHaveText('OEM / ODM');
    await page.goto(base + 'oem-odm/');
    await page.evaluate(() => document.fonts.ready);
    await expect(page.locator('main')).toHaveCSS('background-color', 'rgb(248, 244, 236)');
    const band = page.locator('.matrix-band');
    const matrix = band.locator('.matrix');
    await expect(band).toHaveCSS('background-color', 'rgba(0, 0, 0, 0)');
    await expect(matrix).toHaveCSS('border-radius', '20px');
    await expect(matrix).toHaveCSS('background-color', 'rgb(239, 231, 214)');
    const panel = (await matrix.boundingBox())!;
    const gutter = width === 390 ? 24 : 48;
    expect(panel.x).toBe(gutter);
    expect(width - panel.x - panel.width).toBe(gutter);
    for (const group of ['.capability-panel', '.brief-panel']) {
      await expect(matrix.locator(group)).toHaveCSS('background-color', 'rgba(0, 0, 0, 0)');
    }
    if (width < 1440) {
      await expect(page.locator('main h1')).toHaveCSS('font-size', width === 390 ? '26px' : '36px');
      await expect(page.locator('.workflow h3').first()).toHaveCSS('font-size', width === 390 ? '20px' : '24px');
    }
    await expectAligned(page.locator('[data-techniques]'), 'figure', ['img', 'h3', 'p']);
    await expectAligned(page.locator('.workflow'), 'article', ['img', 'h3', 'p']);
    await page.goto(base + (locale === 'en' ? '404.html' : '404/'));
    await expect(page.locator('main')).toHaveCSS('background-color', 'rgb(248, 244, 236)');
    await expectAligned(page.locator('.recovery-grid'), 'a', ['span', 'p']);
    await expect(page.locator('.error-code')).toHaveCSS('font-family', /Literata/);
    await expect(page.locator('.recovery-card > span').first()).toHaveCSS('border-bottom-width', '0px');
    if (locale === 'ar' && width === 390) {
      await page.goto(base + 'sampling/');
      await expect(page.locator('main h1')).toHaveCSS('font-size', '26px');
      await expect(page.locator('.capabilities-strip')).toHaveCSS('border-radius', '0px');
      await expect(page.locator('.intro + .capabilities-strip')).toHaveCSS('width', '390px');
    }
    await page.goto(base + 'collections/');
    await expect(page.locator('.quote-float')).toHaveCount(0);
  });
}
