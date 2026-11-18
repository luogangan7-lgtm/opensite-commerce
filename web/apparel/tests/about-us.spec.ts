/** CP-ABOUT production-build journey; independent locale/fact expectations and real image geometry. */
import { test, expect } from '@playwright/test';
import { createHash } from 'node:crypto';
import media from '../about-media.json' with { type: 'json' };

for (const locale of ['en','zh','es','fr','de','pt','ru','ar']) {
  const prefix = locale === 'en' ? '' : `/${locale}`;
  test(`About us ${locale} preserves facts, direction and reciprocal links`, async ({ page }) => {
    const response = await page.goto(`${prefix}/about-us/`);
    expect(response?.status()).toBe(200);
    await expect(page.locator('html')).toHaveAttribute('lang', locale);
    await expect(page.locator('html')).toHaveAttribute('dir', locale === 'ar' ? 'rtl' : 'ltr');
    await expect(page.locator('main h1')).toHaveCount(1);
    const main = page.locator('main');
    await expect(main).toContainText('Hunan Xuanliang Eco-Technology Co., Ltd.');
    await expect(main).toContainText('湖南旋粮生态科技有限公司');
    await expect(main.locator('address')).toContainText('1401');
    await expect(main.locator('address')).toContainText('B8');
    await expect(main).toContainText(/50|٥٠/);
    await expect(main).toContainText(/1[,\.\s]?000|١[٬،]?٠٠٠/);
    await expect(main).toContainText('EXW');
    await expect(main).toContainText('FOB');
    expect(await main.innerText()).not.toMatch(/AI[- ]generated|AI illustration|Illustrative image|AI 生成|صورة توضيحية/i);
    await expect(page.locator('link[rel="alternate"]')).toHaveCount(9);
    await expect(page.locator('link[rel="alternate"][hreflang="ar"]')).toHaveAttribute('href', /\/ar\/about-us\/$/);
    const footer = page.locator(`footer a[href="${prefix}/about-us/"]`);
    await footer.click();
    await expect(page).toHaveURL(new RegExp(`${prefix}/about-us/$`));
  });
}

for (const width of [1440,768,390]) {
  test(`About intro retains top headroom and column alignment at ${width}`, async ({ page }) => {
    await page.setViewportSize({width,height:1000});
    await page.goto('/about-us/');
    const image = page.locator('[data-about-intro-image] img');
    await expect(image).toHaveCSS('object-position', '50% 0%');
    await expect(image).toHaveCSS('object-fit', 'cover');
    // Top-aligned cover preserves source row 0 at every breakpoint; the SHA-bound
    // approved portrait includes the headroom verified in the Figma before/after.
    const edges = await page.locator('.about-intro-row').evaluate(row => {
      const title = row.querySelector('h2')!.getBoundingClientRect();
      const link = row.querySelector('a')!.getBoundingClientRect();
      return {title:title.left,link:link.left,overflow:document.documentElement.scrollWidth-innerWidth};
    });
    expect(Math.abs(edges.title - edges.link)).toBeLessThanOrEqual(1);
    expect(edges.overflow).toBeLessThanOrEqual(1);
  });
  // Like imagery.spec.ts, actual image bytes require a packaged artifact, never the raw CI build.
  test(`packaged About portrait decodes and retains SHA-bound headroom at ${width}`, async ({ page, request }) => {
    test.skip(!process.env.APPAREL_PREVIEW_URL && process.env.APPAREL_PACKAGED_DIST !== '1', 'Requires packaged preview or local CI-artifact packaging acceptance');
    const response = await request.get('/deploy-manifest.json');
    expect(response.status()).toBe(200);
    const manifest = await response.json();
    expect(manifest.sha).toBe(process.env.APPAREL_PACKAGED_SHA || process.env.GITHUB_SHA);
    const portrait = media.find(image => image.id === 'collections')!;
    expect(manifest.aboutMedia.find((image: {id:string}) => image.id === portrait.id).sha256).toBe(portrait.sha256);
    await page.setViewportSize({width,height:1000});
    await page.goto('/about-us/');
    const image = page.locator('[data-about-intro-image] img');
    await image.scrollIntoViewIfNeeded();
    await expect.poll(() => image.evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth > 0)).toBe(true);
    await expect(image).toHaveCSS('object-position', '50% 0%');
    await expect(image).toHaveCSS('object-fit', 'cover');
    const src = await image.evaluate((img: HTMLImageElement) => new URL(img.currentSrc).pathname);
    const delivered = manifest.files.find((file: {url:string}) => file.url === src);
    expect(delivered.sourceSha256).toBe(portrait.sha256);
    const bytes = await request.get(src);
    expect(bytes.status()).toBe(200);
    expect(createHash('sha256').update(await bytes.body()).digest('hex')).toBe(delivered.sha256);
  });
}

for (const locale of ['en','ar']) {
  const prefix = locale === 'en' ? '' : '/ar';
  test(`About navigation ${locale} works on desktop and mobile`, async ({ page }) => {
    await page.setViewportSize({width:1440,height:1000});
    await page.goto(`${prefix}/about-us/`);
    const group = page.locator('.about-navigation');
    await group.locator('summary').click();
    await expect(group).toHaveAttribute('open', '');
    await group.locator(`a[href="${prefix}/about/"]`).click();
    await expect(page).toHaveURL(new RegExp(`${prefix}/about/$`));
    await group.locator('summary').click();
    await page.keyboard.press('Escape');
    await expect(group).not.toHaveAttribute('open', '');
    await group.locator('summary').click();
    await group.locator(`a[href="${prefix}/about-us/"]`).click();
    await expect(page).toHaveURL(new RegExp(`${prefix}/about-us/$`));
    await page.setViewportSize({width:390,height:844});
    await page.locator('[data-menu-toggle]').click();
    await expect(page.locator(`#mobile-navigation > nav a[href="${prefix}/about/"]`)).toBeVisible();
    await page.locator(`#mobile-navigation > nav a[href="${prefix}/about-us/"]`).click();
    await expect(page).toHaveURL(new RegExp(`${prefix}/about-us/$`));
  });
}
