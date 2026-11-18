/** WAVE9 F3 PDP and Home sibling geometry at the three acceptance widths. */
import { expect, test, type Locator } from '@playwright/test';

const delta = (values: number[]) => Math.max(...values) - Math.min(...values);
const boxes = async (items: Locator) => {
  const result = await Promise.all((await items.all()).map(item => item.boundingBox()));
  return result.filter(box => box !== null);
};

for (const width of [390, 768, 1440]) for (const locale of ['en', 'ar', 'zh']) {
  test(`${locale} ${width}: sibling rows and carousel controls`, async ({ page }) => {
    const prefix = locale === 'en' ? '' : '/' + locale;
    await page.setViewportSize({ width, height: 900 });
    await page.goto(prefix + '/products/cotton430-hoodie-58065/');
    await page.evaluate(() => document.fonts.ready);
    const gallery = page.locator('[data-gallery-tile]:visible');
    await expect(gallery).toHaveCount(3);
    const photos = await boxes(gallery.locator('img'));
    const captions = await boxes(gallery.locator('figcaption'));
    expect.soft(delta(photos.map(photo => photo.height)), 'secondary photo height').toBeLessThanOrEqual(2);
    expect.soft(delta(photos.map(photo => photo.width)), 'secondary photo width').toBeLessThanOrEqual(2);
    expect.soft(delta(captions.map(caption => caption.y)), 'secondary caption baseline').toBeLessThanOrEqual(2);
    const spec = await boxes(page.locator('[data-full-spec] td'));
    expect.soft(delta(spec.map(cell => cell.x)), 'spec value column').toBeLessThanOrEqual(2);

    await page.goto(prefix + '/');
    await page.evaluate(() => document.fonts.ready);
    const category = page.locator('[data-home-category]');
    for (const selector of ['img', 'h3']) {
      const tileParts = await boxes(category.locator(selector));
      expect(tileParts).toHaveLength(4);
      expect.soft(delta(tileParts.map(part => part.height)), `category ${selector} height`).toBeLessThanOrEqual(2);
    }
    const panels = await boxes(page.locator('[data-home-model]'));
    const buttons = await boxes(page.locator('[data-home-model] .button'));
    expect(panels).toHaveLength(3);
    expect.soft(delta(panels.map(panel => panel.height)), 'ways to buy panel height').toBeLessThanOrEqual(2);
    expect.soft(delta(buttons.map((button, i) => panels[i].y + panels[i].height - button.y - button.height)), 'ways to buy CTA inset').toBeLessThanOrEqual(2);
    if (width <= 768) {
      const dots = await page.locator('.hero-dots').boundingBox();
      expect(dots).not.toBeNull();
      expect(Math.abs(dots!.x + dots!.width / 2 - width / 2)).toBeLessThanOrEqual(2);
    }
    const [counter, pause] = await Promise.all(['.hero-counter', '[data-playback]'].map(selector => page.locator(selector).boundingBox()));
    expect(counter && pause && (counter.x + counter.width <= pause.x || pause.x + pause.width <= counter.x || counter.y + counter.height <= pause.y || pause.y + pause.height <= counter.y)).toBe(true);
  });
}
