/** CTA-07/08 browser contract: localized PDP links preserve product and distinct sample intent. */
import { test, expect } from '@playwright/test';
for (const locale of ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar']) {
  for (const width of [390, 1440]) for (const slug of ['apparel-59043', 'cotton430-hoodie-58065']) {
    test(`${locale} ${width} ${slug} quote and sample`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 });
      const prefix = locale === 'en' ? '' : `/${locale}`;
      for (const sample of [false, true]) {
        await page.goto(`${prefix}/products/${slug}/`);
        await page.locator('.product-info .actions a').nth(sample ? 1 : 0).click();
        await expect(page).toHaveURL(`${test.info().project.use.baseURL}${prefix}/contact/?product=${slug}${sample ? '&type=sample' : ''}#rfq-form`);
        await expect(page.locator('#rfq-form')).toBeVisible();
        await expect(page.locator('[name="product_slug"]')).toHaveValue(slug);
        await expect(page.locator('[name="message"]')).toBeVisible();
        await expect(page.locator('[name="message"]')).toHaveValue(new RegExp(slug));
        const message = await page.locator('[name="message"]').inputValue();
        expect(message.split('\n').includes('sample')).toBe(sample);
      }
    });
  }
}
