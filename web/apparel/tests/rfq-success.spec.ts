/** C2 browser acceptance: intercepted synthetic submit only; localized Sent → collection. */
import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
for (const locale of ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar'] as const) {
  for (const width of [390, 768, 1440]) {
    test(`Sent confirmation and return ${locale} ${width}`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 });
      let submits = 0;
      await page.route('**/api/rfq', async route => {
        submits++;
        expect(route.request().method()).toBe('POST');
        await route.fulfill({ status: 201, contentType: 'application/json', body: '{}' });
      });
      const prefix = locale === 'en' ? '' : `/${locale}`;
      await page.goto(`${prefix}/contact/`);
      await page.locator('[name="name"]').fill('CI');
      await page.locator('[name="company"]').fill('Example');
      await page.locator('[name="email"]').fill('ci@example.invalid');
      await page.locator('[name="message"]').fill('Synthetic C2 request');
      await page.locator('button[type="submit"]').click();
      await expect(page).toHaveURL(`${prefix}/contact/?sent=1`);
      const confirmation = page.locator('[data-rfq-success]');
      await expect(confirmation).toBeVisible();
      await expect(confirmation).toBeFocused();
      // Require the buyer-visible content; section border can round beyond the scroll pixel.
      await expect(confirmation.getByRole('heading')).toBeInViewport({ ratio: 1 });
      await expect(confirmation.locator('p')).toBeInViewport({ ratio: 1 });
      await expect(confirmation).toContainText(JSON.parse(readFileSync(new URL(`../src/i18n/${locale}.json`, import.meta.url), 'utf8')).success);
      await expect(page.locator('html')).toHaveAttribute('dir', locale === 'ar' ? 'rtl' : 'ltr');
      const back = confirmation.getByRole('link');
      await expect(back).toBeInViewport({ ratio: 1 });
      await expect(back).toHaveText(JSON.parse(readFileSync(new URL(`../src/i18n/${locale}.json`, import.meta.url), 'utf8')).backToProducts);
      await expect(back).toHaveAttribute('href', `${prefix}/collections/`);
      await page.keyboard.press('Tab');
      await expect(back).toBeFocused();
      await page.keyboard.press('Enter');
      await expect(page).toHaveURL(`${prefix}/collections/`);
      expect(submits).toBe(1);
    });
  }
}
