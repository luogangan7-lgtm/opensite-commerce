/** Detail regressions for POLISH; asserts rendered geometry in the production build. */
import { test, expect } from '@playwright/test';

for (const width of [1440, 768, 390]) {
  test(`POLISH CTA and portrait geometry ${width}`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 });
    for (const path of ['/', '/collections/']) {
      await page.goto(path);
      const gaps = await page.locator('a.button:has(> span[aria-hidden="true"])').evaluateAll(nodes => nodes.map(node => {
        const arrow = node.querySelector('span[aria-hidden="true"]')!.getBoundingClientRect();
        const range = document.createRange(); range.selectNodeContents(node.firstChild!);
        const text = range.getBoundingClientRect();
        return Math.max(arrow.left - text.right, text.left - arrow.right);
      }));
      if(path==='/') expect(gaps.length).toBeGreaterThan(0);
      else {await expect(page.locator('.shop-quote').first()).not.toBeEmpty();await expect(page.locator('.shop-quote span[aria-hidden]')).toHaveCount(0);}
      for (const gap of gaps) expect(gap).toBeGreaterThan(0);
      if (path === '/') await expect(page.locator('[data-home-category] img').first()).toHaveCSS('object-fit', 'cover');
      else await expect(page.locator('.shop-photo > a > img:first-child').first()).toHaveCSS('object-position', /^50% 0(?:px|%)$/);
    }
  });
  test(`POLISH PDP frame and thumbnail rail ${width}`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 });
    await page.goto('/products/hoodie-set-k02/');
    const image = page.locator('[data-gallery-main]');
    const box = (await image.boundingBox())!;
    expect(Math.abs(box.width / box.height / 0.8 - 1)).toBeLessThanOrEqual(0.02);
    await expect(page.locator('.gallery-actions, [data-gallery-prev], [data-gallery-next], [data-gallery-zoom]')).toHaveCount(0);
    const rail = page.locator('.gallery-thumbs:not([hidden])');
    if (width >= 768) {
      expect(await rail.evaluate(node => {
        const links = [...node.querySelectorAll('a')];
        return new Set(links.map(link => link.offsetTop)).size === links.length;
      })).toBe(true);
      const last = rail.locator('a').last();
      await last.scrollIntoViewIfNeeded();
      await expect(last).toBeInViewport();
    } else await expect(page.locator('.grouped-rail')).toBeHidden();
    await expect(page.locator('.moq-line')).toHaveText('MOQ 50 pcs · mix colours & sizes');
  });
}
