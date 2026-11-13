/** C5 HDR/MENU browser gate; built Shell at mobile and existing desktop breakpoints. */
import { test, expect } from '@playwright/test';
for (const locale of ['en', 'zh', 'ar']) {
  const prefix = locale === 'en' ? '' : `/${locale}`;
  test(`mobile navigation ${locale}`, async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`${prefix}/`);
    const toggle = page.locator('[data-menu-toggle]');
    const menu = page.locator('#mobile-navigation');
    await expect(toggle).toBeVisible();
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await toggle.click();
    await expect(menu).toBeVisible();
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect(page.locator('main')).toHaveAttribute('inert', '');
    const first = menu.locator('nav a').first();
    await expect(first).toBeFocused();
    await expect(first).toHaveAttribute('aria-current', 'page');
    expect(await page.locator('header').evaluate(element => Number(getComputedStyle(element).zIndex))).toBeGreaterThan(await page.locator('.quote-float').evaluate(element => Number(getComputedStyle(element).zIndex)));
    for (const route of ['/', '/collections/', '/oem-odm/', '/about/', '/contact/']) {
      await expect(menu.locator(`:scope > nav a[href="${prefix}${route}"]`)).toBeVisible();
    }
    await expect(menu.locator(`a[href="${prefix}/contact/#rfq-form"]`)).toBeVisible();
    await page.screenshot({ path: `evidence/c5-menu-${locale}.png` });
    await menu.locator('summary').click();
    for (const lang of ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar']) {
      await expect(menu.locator(`a[lang="${lang}"]`)).toBeVisible();
    }
    for (const target of await menu.locator('a, button, summary').all()) {
      const box = await target.boundingBox();
      expect(box?.width).toBeGreaterThanOrEqual(44);
      expect(box?.height).toBeGreaterThanOrEqual(44);
    }
    const last = menu.locator('a[lang="ar"]');
    await last.focus(); await page.keyboard.press('Tab');
    await expect(menu.locator('[data-menu-close]')).toBeFocused();
    await page.keyboard.press('Shift+Tab'); await expect(last).toBeFocused();
    await page.keyboard.press('Escape');
    await expect(menu).not.toBeVisible(); await expect(toggle).toBeFocused();
    await toggle.click(); await menu.locator('[data-menu-close]').click();
    await expect(menu).not.toBeVisible(); await expect(toggle).toBeFocused();
    await toggle.click();
    await page.mouse.click(5, 820);
    await expect(menu).not.toBeVisible(); await expect(toggle).toBeFocused();
    await toggle.click(); await toggle.click();
    await expect(menu).not.toBeVisible(); await expect(toggle).toBeFocused();
    await toggle.click();
    await page.setViewportSize({ width: 768, height: 1000 });
    await expect(menu).not.toBeVisible();
    await expect(page.locator('main')).not.toHaveAttribute('inert', '');
    await page.setViewportSize({ width: 390, height: 844 });
    await toggle.click();
    const rect = await menu.boundingBox(); expect(rect!.x).toBeGreaterThanOrEqual(0);
    expect(rect!.x + rect!.width).toBeLessThanOrEqual(390);
    await expect(menu).toHaveCSS('direction', locale === 'ar' ? 'rtl' : 'ltr');
    await menu.locator(`:scope > nav a[href="${prefix}/collections/"]`).click();
    await expect(page).toHaveURL(new RegExp(`${prefix}/collections/$`));
    await expect(page.locator('h1')).toBeVisible();
  });
  for (const width of [768, 1440]) test(`desktop navigation ${locale} ${width}`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 }); await page.goto(`${prefix}/`);
    await expect(page.locator('[data-menu-toggle]')).not.toBeVisible();
    await expect(page.locator('.header-inner > nav a')).toHaveCount(5);
    for (const link of await page.locator('.header-inner > nav a').all()) await expect(link).toBeVisible();
    await expect(page.locator('.header-inner > .button')).toBeVisible();
    await expect(page.locator('.header-inner > .language-switcher')).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  });
}
