/** CP-PDP-FB-t2: root 1:1 gallery polish — sr-only live region, Figma tab pills, filled rail thumbs. */
import { test, expect } from '@playwright/test';

for (const path of ['/products/hoodie-set-k02/', '/ar/products/hoodie-set-k02/']) {
  test(`${path}: live region is sr-only`, async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(path);
    const status = page.locator('[data-gallery-status]');
    await expect(status).toHaveCount(1);
    const box = (await status.boundingBox())!;
    expect(box.width, 'status width').toBeLessThanOrEqual(1);
    expect(box.height, 'status height').toBeLessThanOrEqual(1);
  });

  test(`${path}: inactive tabs are filled pills and all labels are regular weight`, async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(path);
    const tabs = page.locator('[data-gallery-view]');
    await expect(tabs).toHaveCount(3);
    for (const tab of await tabs.all()) {
      const weight = await tab.evaluate(node => getComputedStyle(node).fontWeight);
      expect(weight, 'tab font-weight').toBe('400');
      if (await tab.getAttribute('aria-selected') !== 'true') {
        const bg = await tab.evaluate(node => getComputedStyle(node).backgroundColor);
        expect(bg, 'inactive tab background').not.toBe('rgba(0, 0, 0, 0)');
      }
    }
  });

  test(`${path}: rail thumbnails fill their 4:5 frame`, async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(path);
    const thumbs = page.locator('.gallery-thumbs img');
    expect(await thumbs.count()).toBeGreaterThan(0);
    for (const thumb of await thumbs.all()) await expect(thumb).toHaveCSS('object-fit', 'cover');
  });
}
