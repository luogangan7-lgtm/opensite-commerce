/** Browser regression for §6.2/6.3: PDP interaction, filled details and uncropped viewer. */
import { test, expect } from '@playwright/test';
test.beforeEach(async({page})=>{await page.setViewportSize({width:1440,height:1000})});
for (const locale of ['en','zh','es','fr','de','pt','ru','ar']) test(`gallery details ${locale}`, async ({ page }) => {
 await page.goto(`${locale === 'en' ? '' : '/' + locale}/products/crew-fleece-8900-31/`);
 const links = page.locator('.gallery-thumbs:not([hidden]) a');
 const roles = await links.evaluateAll(nodes => nodes.map(n => (n as HTMLElement).dataset.role));
 const rank: Record<string, number> = {front:0,back:1,turn:2,garment:3,print:4,structure:5,fabric:6};
 const known = roles.filter(r => r && r in rank).map(r => rank[r!]);
 expect(known).toEqual([...known].sort((a,b)=>a-b));
 const detail = page.locator('.gallery-thumbs a[data-role="fabric"]');
 await expect(detail).toHaveCount(1);
 await detail.click();
 const main = page.locator('[data-gallery-main]');
 await expect(main).toHaveCSS('object-fit', 'cover');
 if (process.env.APPAREL_PACKAGED_IMAGES === '1' || process.env.APPAREL_PREVIEW_URL) await expect.poll(() => main.evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth === 1200 && img.naturalHeight === 1200)).toBe(true);
 const box = await main.boundingBox(); expect(box!.width / box!.height).toBeCloseTo(4/5, 2);
 await expect(page.locator('[data-digital-label]')).toHaveCount(0);
 await expect(main).toHaveAttribute('alt', /.+/);
 await main.click();
 await expect(page.locator('dialog')).toBeVisible();
 await expect(page.locator('dialog')).toHaveAttribute('aria-label', await main.getAttribute('alt') || '');
 await expect(page.locator('dialog img')).toHaveCSS('object-fit', 'contain');
 await page.keyboard.press('Escape');
 await links.first().click();
 await expect(page.locator('[data-digital-label]')).toHaveCount(0);
});

test('primary colour sequence excludes other fronts and unknown roles until swatch selection', async ({page}) => {
 await page.goto('/products/crew-fleece-8900-31/');
 const visible = page.locator('.gallery-thumbs:not([hidden]) a');
 await expect(visible.locator('[data-role="overview"]')).toHaveCount(0);
 expect(await visible.evaluateAll(nodes=>nodes.every(n=>(n as HTMLElement).dataset.colour==='cream'))).toBe(true);
 await expect(page.locator('.gallery-thumbs a[data-role="overview"], .gallery-thumbs a[data-role="model"]')).toHaveCount(0);
 await expect(page.locator('.gallery-thumbs:not([hidden]) a[data-role="front"]')).toHaveCount(1);
 await page.locator('[data-gallery-swatch="black"]').click();
 await expect(visible).toHaveCount(1);
 await expect(visible).toHaveAttribute('data-role','front');
 await expect(visible).toHaveAttribute('data-colour','black');
 await expect(page.locator('[data-gallery-main]')).toHaveAttribute('src', await visible.getAttribute('href') || '');
 await expect(page.locator('.gallery-thumbs:not([hidden]) a[data-role="fabric"]')).toHaveCount(0);
 await page.locator('[data-gallery-swatch="cream"]').click();
 await expect(page.locator('.gallery-thumbs:not([hidden]) a[data-role="fabric"]')).toHaveCount(1);
});
