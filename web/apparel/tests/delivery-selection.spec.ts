/** Delivery policy: only narrow mobile viewports may select the smaller candidate. */
import { test, expect } from '@playwright/test';
for (const width of [412, 767, 768, 1024, 1440]) for (const deviceScaleFactor of [1, 1.75]) test(`PDP delivery ${width} DPR ${deviceScaleFactor}`, async ({ browser, baseURL }) => {
 const context = await browser.newContext({ baseURL, viewport: {width,height:900},deviceScaleFactor });
 const page = await context.newPage();
 await page.goto('/products/hoodie-set-k02/');
 const main = page.locator('[data-gallery-main]');
 const expected = width < 768 ? '/digital-front-640.webp' : '/digital-front.webp';
 await expect.poll(() => main.evaluate((img: HTMLImageElement) => img.currentSrc)).toContain(expected);
 await page.locator('.gallery-thumbs:not([hidden]) a[data-role="fabric"]').click();
 await expect.poll(() => main.evaluate((img: HTMLImageElement) => img.currentSrc)).toContain('/fabric.webp');
 await context.close();
});
