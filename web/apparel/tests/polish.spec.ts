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
      const portrait = page.locator(path === '/' ? '.wholesale-photo img' : '.shop-photo > a > img:first-child');
      await expect(portrait.first()).toHaveCSS('object-position', /^50% 0(?:px|%)$/);
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
    const geometry = await rail.evaluate(node => {
      const links = [...node.querySelectorAll('a')];
      const rect = node.getBoundingClientRect();
      return { links:links.length, rows: new Set(links.map(a => a.offsetTop)).size, height: node.clientHeight, content: node.scrollHeight,
        inside: links.every(a => { const r = a.getBoundingClientRect(); return r.top >= rect.top && r.bottom <= rect.bottom; }) };
    });
    if(width>=768)expect(geometry.rows).toBe(geometry.links);else await expect(page.locator('.grouped-rail')).toBeHidden();
    expect(geometry.inside).toBe(true);
    expect(geometry.content).toBeLessThanOrEqual(geometry.height);
    await expect(page.locator('.moq-line')).toHaveText('MOQ 50 pcs · mix colours & sizes');
    await expect(page.locator('[data-quick-spec]')).toContainText('50 pcs');
  });
}
