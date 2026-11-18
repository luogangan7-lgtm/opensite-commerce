/** L1 acceptance against served build: complete localized copy, reciprocal SEO and legal boundaries. */
import { test, expect } from '@playwright/test';
import legalCopy from '../src/content/launch-pages.json' with { type: 'json' };
const languages = ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar'];
const routes = ['oem-odm', 'sampling', 'shipping', 'privacy', 'terms'];
// Owner-confirmed launch date, independently asserted against rendered localized labels.
const launchDates: Record<string, string> = { en:'November 15, 2026', zh:'2026年11月15日', es:'15 de noviembre de 2026', fr:'15 novembre 2026', de:'15. November 2026', pt:'15 de novembro de 2026', ru:'15 ноября 2026 г.', ar:'15 نوفمبر 2026' };
for (const locale of languages) for (const route of routes) {
  test(`launch page ${locale}/${route}`, async ({ page, request }) => {
    const path = `${locale === 'en' ? '' : '/' + locale}/${route}/`;
    const response = await page.goto(path);
    expect(response?.status()).toBe(200);
    await expect(page.locator('html')).toHaveAttribute('lang', locale);
    await expect(page.locator('html')).toHaveAttribute('dir', locale === 'ar' ? 'rtl' : 'ltr');
    const headings = {privacy:5,terms:5,'oem-odm':6,sampling:5,shipping:5};
    await expect(page.locator('[data-service-page] h2')).toHaveCount(headings[route as keyof typeof headings]);
    const text = await page.locator('[data-service-page]').innerText();
    expect(text.length).toBeGreaterThan(300);
    expect(text).not.toMatch(/placeholder|coming soon|lorem ipsum|待 owner 批准/i);
    await expect(page.locator('[data-english-fallback]')).toHaveCount(0);
    for (const language of languages) {
      const target = `${language === 'en' ? '' : '/' + language}/${route}/`;
      const alternate = page.locator(`link[hreflang="${language === 'zh' ? 'zh-Hans' : language}"]`);
      expect(new URL(await alternate.getAttribute('href') || '').pathname).toBe(target);
    }
    const sitemap = await request.get('/sitemap.xml');
    expect(sitemap.status()).toBe(200);
    expect(await sitemap.text()).toContain(`${path}</loc>`);
    if (route === 'privacy' || route === 'terms') {
      expect(text).toContain('sales@sourcyowear.com');
      expect(text).toContain('91430105MA7J4C1WX7');
      await expect(page.locator('.legal-updated')).toContainText(launchDates[locale]);
      expect(text).not.toContain('OWNER-INPUT');
      expect(await page.locator('.service-copy > div > p > [data-legal-text]').allTextContents()).toEqual(legalCopy[locale as keyof typeof legalCopy][route as 'privacy' | 'terms'][2].map(section => section[1]));
      await expect(page.locator('#rfq-form')).toHaveCount(0);
    }
    if (route === 'privacy') {
      expect(text).toContain('Google LLC'); expect(text).toContain('Supabase');
      expect(text).toContain('Consent Mode v2');
    }
  });
}

// Exercise the actual Pages asset router, including deep misses; Python's static server cannot prove this.
for (const locale of languages) test(`native Pages missing routes ${locale}`, async ({ request }) => {
  const origin = process.env.APPAREL_PREVIEW_URL || `http://127.0.0.1:${Number(process.env.APPAREL_TEST_PORT || 4322) + 1}`;
  const prefix = locale === 'en' ? '' : '/' + locale;
  for (const path of ['/missing-legal-test', '/products/missing-legal-test/deeper/']) {
    const response = await request.get(origin + prefix + path);
    expect(response.status()).toBe(404);
    const body = await response.text();
    expect(body).toContain(`lang="${locale}" dir="${locale === 'ar' ? 'rtl' : 'ltr'}"`);
    expect([...body.matchAll(/data-recovery-card/g)]).toHaveLength(4);
    expect(body).toContain(`href="${prefix}/"`);
  }
});

for (const locale of ['en', 'zh', 'ar']) for (const width of [1440, 768, 390]) test(`legal recovery layout ${locale} ${width}`, async ({ page }) => {
  await page.setViewportSize({ width, height: 900 });
  for (const route of ['privacy', 'terms', '404']) {
    await page.goto(`${locale === 'en' ? '' : '/' + locale}/${route === '404' && locale === 'en' ? '404.html' : route + '/'}`);
    await page.evaluate(() => document.fonts.ready);
    const banner = page.locator('[data-cookie-banner]');
    if (await banner.isVisible()) await page.locator('[data-cookie-choice=essential]').click();
    await expect(banner).toBeHidden();
    await expect(page.locator('h1')).toHaveCSS('font-weight', '400');
    await expect(page.locator('.quote-float')).toHaveCount(0);
    if (route === '404') {
      await expect(page.locator('.error-code')).toHaveCSS('font-size', '88px');
      await expect(page.locator('.recovery-card').first()).toHaveCSS('background-color', 'rgba(0, 0, 0, 0)');
      await expect(page.locator('.recovery-card img')).toHaveCount(0);
    } else {
      await expect(page.locator('h1')).toHaveCSS('font-size', width < 900 ? '36px' : '56px');
      const contents = page.locator('.legal-contents');
      if (width < 900) {
        await expect(contents).toBeVisible();
        await contents.locator('summary').click();
        const firstSection = contents.locator('a').first();
        await expect(firstSection).toHaveAttribute('href', '#section-1');
        await firstSection.click();
        expect(new URL(page.url()).hash).toBe('#section-1');
        await contents.locator('summary').click();
      } else {
        await expect(contents).toBeHidden();
      }
      const column = await page.locator('.service-copy').boundingBox();
      expect(Math.round(column!.x + column!.width / 2)).toBe(width / 2);
      const container = (await page.locator('.legal-page').boundingBox())!;
      const header = (await page.locator('.header-inner').boundingBox())!;
      expect(Math.round(container.x)).toBe(Math.round(header.x));
      expect(Math.round(container.x + container.width)).toBe(Math.round(header.x + header.width));
      const heading = (await page.locator('h1').boundingBox())!;
      expect(Math.round(locale === 'ar' ? heading.x + heading.width : heading.x)).toBe(Math.round(locale === 'ar' ? column!.x + column!.width : column!.x));
      expect(await page.locator('.service-copy > div > p').first().evaluate(el => Math.round(el.getBoundingClientRect().width))).toBe((width < 900 ? 342 : 760));
    }
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await page.screenshot({ path: `evidence/cp-legal-${route}-${locale}-${width}.png`, fullPage: true });
  }
});
