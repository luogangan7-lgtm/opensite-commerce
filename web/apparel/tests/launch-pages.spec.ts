/** L1 acceptance against served build: complete localized copy, reciprocal SEO and legal boundaries. */
import { test, expect } from '@playwright/test';
import legalCopy from '../src/content/launch-pages.json' with { type: 'json' };
const languages = ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar'];
const routes = ['oem-odm', 'sampling', 'shipping', 'privacy', 'terms'];
for (const locale of languages) for (const route of routes) {
  test(`launch page ${locale}/${route}`, async ({ page, request }) => {
    const path = `${locale === 'en' ? '' : '/' + locale}/${route}/`;
    const response = await page.goto(path);
    expect(response?.status()).toBe(200);
    await expect(page.locator('html')).toHaveAttribute('lang', locale);
    await expect(page.locator('html')).toHaveAttribute('dir', locale === 'ar' ? 'rtl' : 'ltr');
    await expect(page.locator('[data-service-page] h2')).toHaveCount(route === 'privacy' ? 5 : route === 'terms' ? 5 : 4);
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
      await expect(page.locator('.legal-updated')).toContainText('OWNER-INPUT');
      expect(await page.locator('.service-copy > div > p').allTextContents()).toEqual(legalCopy[locale as keyof typeof legalCopy][route as 'privacy' | 'terms'][2].map(section => section[1]));
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
