/** CP-PDP-FB root findings: one view control, rail order Front/Back/Side/Details/Fabric with even spacing,
 * and no broken image in any gallery state (EN/AR/ZH at 390/1440). Image decode needs packaged bytes. */
import { test, expect, type Page } from '@playwright/test';
const packaged = process.env.APPAREL_PACKAGED_IMAGES === '1' || !!process.env.APPAREL_PREVIEW_URL;
const slug = 'hoodie-set-k02';
for (const locale of ['en', 'zh', 'ar']) for (const width of [390, 1440]) test(`gallery controls and images ${locale} ${width}`, async ({ page }) => {
  test.setTimeout(120000);
  await page.setViewportSize({ width, height: 900 });
  await page.goto(`${locale === 'en' ? '' : '/' + locale}/products/${slug}/`);
  // One view control: the segmented ViewToggle. No pill, arrows or second tablist.
  await expect(page.locator('[data-fb-gallery] [data-view-toggle]')).toHaveCount(1);
  await expect(page.locator('[data-fb-gallery] [role=tab]')).toHaveCount(3);
  await expect(page.locator('.gallery-actions, [data-gallery-prev], [data-gallery-next], [data-gallery-zoom]')).toHaveCount(0);
  // Rail order and equal spacing between neighbouring thumbnails.
  const rail = page.locator('.gallery-thumbs:not([hidden]) a');
  expect(await rail.evaluateAll(n => n.map(a => (a as HTMLElement).dataset.role))).toEqual(['front', 'back', 'turn', 'structure', 'fabric']);
  if(width<768)await expect(page.locator('.grouped-rail')).toBeHidden();
  const gaps = width<768?[]:await rail.evaluateAll((nodes, vertical) => nodes.slice(1).map((a, i) => {
    const prev = nodes[i].getBoundingClientRect(), next = a.getBoundingClientRect();
    return vertical ? next.top - prev.bottom : Math.abs(next.left - prev.left) - prev.width;
  }), width >= 768);
  if(gaps.length)expect(Math.max(...gaps) - Math.min(...gaps)).toBeLessThanOrEqual(1);
  const swatchOrder=await page.locator('[data-gallery-swatch]').evaluateAll(ns=>ns.map(n=>({colour:(n as HTMLElement).dataset.gallerySwatch,x:n.getBoundingClientRect().x})));
  expect(swatchOrder.map(s=>s.colour)).toEqual(['off-white/very light grey','heather grey','burgundy','blue (denim)','black']);
  expect(locale==='ar'?swatchOrder[0].x>swatchOrder[4].x:swatchOrder[0].x<swatchOrder[4].x).toBe(true);
  if (!packaged) return;
  // Every gallery state: each colour swatch x each view. Fetch status separates missing files from decode failures.
  const swatches = page.locator('[data-gallery-swatch]');
  for (let s = 0; s < await swatches.count(); s++) {
    await swatches.nth(s).click();
    await expect(page.locator('[data-gallery-swatch][aria-pressed=true]')).toHaveCount(1);
    await expect(page.locator('[data-gallery-main]')).toHaveAttribute('src',await swatches.nth(s).locator('img').getAttribute('src')||'');
    await expect(page.locator('p[data-gallery-colour-label]')).toHaveText(await swatches.nth(s).getAttribute('data-gallery-colour-label')||'');
    for (const view of ['pair', 'side', 'details']) {
      await page.locator(`[data-gallery-view=${view}]`).click();
      await expectNoBrokenImages(page);
    }
  }
});
/** Scope: the gallery and the PDP fabric/detail media it drives; related cards are covered by the packaging gate. */
async function expectNoBrokenImages(page: Page) {
  await page.evaluate(async () => { for (let y = 0; y < document.body.scrollHeight; y += 400) { scrollTo(0, y); await new Promise(r => setTimeout(r, 30)); } scrollTo(0, 0); });
  const urls = await page.evaluate(() => [...new Set([...document.querySelectorAll<HTMLImageElement>('[data-fb-gallery] img, [data-pdp-media] img')].filter(i => i.getAttribute('src') && i.getClientRects().length).flatMap(i => [i.currentSrc || i.src]))]);
  for (const url of urls) expect((await page.request.get(url)).ok(), url).toBe(true);
  await expect.poll(() => page.evaluate(() => [...document.querySelectorAll<HTMLImageElement>('[data-fb-gallery] img, [data-pdp-media] img')].filter(i => i.getAttribute('src') && i.getClientRects().length && !(i.complete && i.naturalWidth > 0)).map(i => i.currentSrc)), { timeout: 15000 }).toEqual([]);
}
