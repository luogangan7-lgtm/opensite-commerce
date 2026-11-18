/** C5 HDR/MENU browser gate; built Shell at mobile and existing desktop breakpoints. */
import { test, expect } from '@playwright/test';
// Shared Header: desktop links form one row; approved tablet widths use the menu.
for (const locale of ['en', 'ar']) for (const width of [1440, 1280, 1101, 1024, 900, 768]) {
  test(`header nav baseline ${locale} ${width}`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 });
    const prefix = locale === 'en' ? '' : '/ar';
    for (const route of ['/', '/about-us/']) {
      await page.goto(`${prefix}${route}`);
      await page.evaluate(() => document.fonts.ready);
      const nav = page.locator('.header-inner > nav');
      if (width <= 1100) {
        await expect(nav).toBeHidden();
        const toggle = page.locator('[data-menu-toggle]');
        await expect(toggle).toBeVisible();
        const logo = await page.locator('header .brand').boundingBox(), menu = await toggle.boundingBox();
        expect(Math.abs(logo!.y + logo!.height / 2 - menu!.y - menu!.height / 2)).toBeLessThanOrEqual(1);
      } else {
        const items = nav.locator(':scope > a, :scope > details > summary');
        await expect(items).toHaveCount(5);
        const before = await items.evaluateAll(nodes => nodes.map(n => ({ y: n.getBoundingClientRect().y, h: n.getBoundingClientRect().height })));
        expect(Math.max(...before.map(b => b.y)) - Math.min(...before.map(b => b.y))).toBeLessThanOrEqual(1);
        const textY = await items.evaluateAll(nodes => nodes.map(n => {
          const range = document.createRange(); range.selectNodeContents(n.firstChild!);
          return range.getBoundingClientRect().y;
        }));
        expect(Math.max(...textY) - Math.min(...textY)).toBeLessThanOrEqual(1);
        await nav.locator('summary').click();
        await expect(nav.locator('.about-dropdown')).toBeVisible();
        expect(await items.evaluateAll(nodes => nodes.map(n => ({ y: n.getBoundingClientRect().y, h: n.getBoundingClientRect().height })))).toEqual(before);
        const trigger = await nav.locator('summary').boundingBox(), panel = await nav.locator('.about-dropdown').boundingBox();
        expect(panel!.y).toBeGreaterThanOrEqual(trigger!.y + trigger!.height - 1);
        await page.keyboard.press('Escape');
      }
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    }
  });
}
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
    await expect(page.locator('.quote-float')).toHaveCount(0);
    await expect(page.locator('footer')).toHaveAttribute('inert', '');
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
    await expect(menu).toBeVisible();
    await expect(menu.locator('summary')).toBeFocused();
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
    await page.setViewportSize({ width: 1440, height: 1000 });
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
  for (const width of [1280, 1440]) test(`desktop navigation ${locale} ${width}`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 }); await page.goto(`${prefix}/`);
    await expect(page.locator('[data-menu-toggle]')).not.toBeVisible();
    const nav = page.locator('.header-inner > nav');
    await expect(nav.locator(':scope > a')).toHaveCount(4);
    for (const link of await nav.locator(':scope > a').all()) await expect(link).toBeVisible();
    const about = nav.locator('.about-navigation');
    await expect(about.locator('summary')).toBeVisible();
    await expect(about.locator('a')).toHaveCount(2);
    await expect(about.locator(`a[href="${prefix}/about-us/"]`)).not.toBeVisible();
    await about.locator('summary').click();
    await expect(about.locator(`a[href="${prefix}/about-us/"]`)).toBeVisible();
    await expect(about.locator(`a[href="${prefix}/about/"]`)).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(about).not.toHaveAttribute('open', '');
    await expect(page.locator('.header-inner > .button')).toBeVisible();
    await expect(page.locator('.header-inner > .language-switcher')).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  });
}

// MENU-08 / LANG-13: dismiss the inline list before dismissing its parent menu.
for (const locale of ['en', 'ar']) {
  test(`nested Escape ${locale}`, async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(locale === 'en' ? '/' : '/ar/');
    const toggle = page.locator('[data-menu-toggle]');
    const menu = page.locator('#mobile-navigation');
    const languages = menu.locator('.mobile-languages');
    const summary = languages.locator('summary');
    await toggle.click();
    await summary.click();
    await languages.locator('a[lang="ar"]').focus();
    await page.keyboard.press('Escape');
    await expect(menu).toBeVisible();
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect(languages).not.toHaveAttribute('open', '');
    await expect(summary).toBeFocused();
    await expect(page.locator('main')).toHaveAttribute('inert', '');
    await expect(page.locator('body')).toHaveCSS('overflow', 'hidden');
    await page.keyboard.press('Escape');
    await expect(menu).not.toBeVisible();
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await expect(toggle).toBeFocused();
    await expect(page.locator('main')).not.toHaveAttribute('inert', '');
    await expect(page.locator('body')).not.toHaveCSS('overflow', 'hidden');
  });
}
