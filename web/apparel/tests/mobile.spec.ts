/** Mobile gate; built static pages and Playwright; CI screenshots bind to checkout SHA. */
import { test, expect } from '@playwright/test';
for (const route of ['/', '/collections/crew-fleece/', '/products/crew-fleece-8900-32/']) {
  test(`390px ${route}`, async ({ page }) => {
    await page.goto(route); await expect(page.locator('h1')).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    if (route.startsWith('/products/')) {
      const second = page.locator('.gallery-thumbs a').nth(1); await second.click();
      await expect(page.locator('[data-gallery-main]')).toHaveAttribute('src', (await second.getAttribute('href'))!);
    }
    await page.screenshot({ path: `evidence/${route.replaceAll('/', '_') || 'home'}.png`, fullPage: true });
  });
}
