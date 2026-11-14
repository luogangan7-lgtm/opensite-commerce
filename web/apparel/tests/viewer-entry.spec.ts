/** Main gallery entry must share the existing viewer for mouse, touch and keyboard. */
import { test, expect } from '@playwright/test';
for (const mode of ['click', 'tap', 'Enter'] as const) test(`main image opens viewer: ${mode}`, async ({ browser, baseURL }) => {
 const context = await browser.newContext({ hasTouch: true, baseURL });
 const page = await context.newPage();
 await page.goto('/products/hoodie-set-k02/');
 const main = page.locator('[data-gallery-main]');
 if (mode === 'Enter') { await main.focus(); await main.press('Enter'); }
 else await main[mode]();
 await expect(page.locator('[data-gallery-viewer]')).toBeVisible();
 await expect(page.locator('[data-gallery-viewer] img')).toHaveAttribute('src', await main.evaluate((img: HTMLImageElement) => img.src));
 await page.keyboard.press('Escape');
 await expect(page.locator('[data-gallery-viewer]')).not.toBeVisible();
 await context.close();
});
