/** Responsibility: real published-release SSR/page/SEO/a11y/visual gates.
 * Dependencies: seeded Go read API + standalone Node Astro server; callers: pinned Playwright CI.
 * Invariants: deterministic release content, both inherited viewports, CI-only golden creation.
 */
import { expect, test, type Page } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
const origin = 'http://127.0.0.1:4321';
const seed = [
  { path: '/', title: 'Made for everyday use', description: 'A published example of pages and products.', text: 'Browse our published collection and learn how this sample site is made.', screenshot: 'home' },
  { path: '/about', title: 'About this shop', description: 'About this published shop.', text: 'This page belongs to the published release.', screenshot: 'cms' },
  { path: '/guides/setup', title: 'About this shop', description: 'About this published shop.', text: 'This page belongs to the published release.', screenshot: 'cms-nested' },
  { path: '/products', title: 'Products', description: 'A published example of pages and products.', text: 'Canvas tote', screenshot: 'products' },
  { path: '/products/canvas-tote', title: 'Canvas tote', description: 'A reusable cotton tote for daily essentials.', text: '$24.00', screenshot: 'product' },
] as const;
async function quality(page: Page) {
  await page.evaluate(async () => { await document.fonts.ready; });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  const report = await new AxeBuilder({ page }).analyze();
  expect(report.violations.filter(item => ['serious', 'critical'].includes(item.impact ?? ''))).toEqual([]);
}
for (const item of seed) test(`storefront SSR ${item.path}`, async ({ page, request }) => {
  // Request HTML independently proves published content exists before browser hydration.
  const html = await request.get(`${origin}${item.path}`);
  expect(html.status()).toBe(200);
  expect(await html.text()).toContain(item.title);
  expect(html.headers()['cache-control']).toContain('no-store');
  const response = await page.goto(`${origin}${item.path}`);
  expect(response?.status()).toBe(200);
  await expect(page.locator('[data-storefront-ready="true"]')).toBeVisible();
  await expect(page.getByRole('heading', { level: 1 })).toHaveText(item.title);
  await expect(page.getByRole('main')).toContainText(item.text);
  await expect(page).toHaveTitle(`${item.title} | OpenSite sample shop`);
  await expect(page.locator('meta[name="description"]')).toHaveAttribute('content', item.description);
  await expect(page.locator('link[rel="canonical"]')).toHaveAttribute('href', `${origin}${item.path}`);
  await expect(page.getByRole('navigation')).toContainText('About');
  await quality(page);
  await expect(page).toHaveScreenshot(`storefront-${item.screenshot}.png`, { fullPage: true });
});

test('storefront published navigation, product link and keyboard skip', async ({ page }) => {
  await page.goto(origin);
  await page.keyboard.press('Tab');
  await expect(page.getByRole('link', { name: 'Skip to content' })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('main')).toBeFocused();
  await page.getByRole('navigation').getByRole('link', { name: 'About', exact: true }).click();
  await expect(page).toHaveURL(`${origin}/about`);
  await page.getByRole('navigation').getByRole('link', { name: 'Products', exact: true }).click();
  await page.getByRole('link', { name: 'Canvas tote', exact: true }).click();
  await expect(page).toHaveURL(`${origin}/products/canvas-tote`);
  await expect(page.getByRole('main')).toContainText('$24.00');
  await page.getByRole('link', { name: 'Back to products' }).click();
  await expect(page).toHaveURL(`${origin}/products`);
});

test('storefront canonical preserves admitted origin for double-slash request paths', async ({ page }) => {
  for (const path of ['//external.example/path', '///external.example/path']) {
    const response = await page.goto(`${origin}${path}?ignored=query#fragment`);
    expect(response?.status()).toBe(404);
    expect(response?.headers()['cache-control']).toContain('no-store');
    // Astro normalizes repeated leading slashes before rendering; authority must stay admitted.
    await expect(page.locator('link[rel="canonical"]')).toHaveAttribute('href', `${origin}/external.example/path`);
    await expect(page.locator('meta[name="robots"]')).toHaveAttribute('content', 'noindex');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText('Page not found');
  }
});

test('storefront missing CMS/product/native routes return accessible 404', async ({ page }) => {
  for (const path of ['/missing', '/products/missing', '/unknown/nested', '/404']) {
    expect((await page.goto(`${origin}${path}`))?.status()).toBe(404);
    await expect(page.getByRole('heading', { level: 1 })).toHaveText('Page not found');
    await expect(page.locator('[data-storefront-ready]')).toHaveCount(0);
    await expect(page.locator('meta[name="robots"]')).toHaveAttribute('content', 'noindex');
    await quality(page);
  }
  await expect(page).toHaveScreenshot('storefront-404.png', { fullPage: true });
});

test('storefront custom 502 page and actual backend failure have usable retry island', async ({ page }) => {
  expect((await page.goto(`${origin}/502`))?.status()).toBe(502);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Store temporarily unavailable');
  await quality(page);
  await expect(page).toHaveScreenshot('storefront-502.png', { fullPage: true });
  // A second standalone process points at a refused API origin; this exercises real server-side fetch failure.
  expect((await page.goto('http://127.0.0.1:4322/'))?.status()).toBe(502);
  await expect(page.locator('[data-storefront-ready]')).toHaveCount(0);
  const retry = page.getByRole('button', { name: 'Try again' });
  await expect(retry).toBeEnabled();
  const [response] = await Promise.all([
    page.waitForResponse(response => response.url() === 'http://127.0.0.1:4322/' && response.request().isNavigationRequest()),
    retry.click(),
  ]);
  expect(response.status()).toBe(502);
  await quality(page);
});

test('storefront SSR responses carry a strict hash-based CSP and pages run without violations', async ({ page, request }) => {
  for (const [path, status] of [['/', 200], ['/products/canvas-tote', 200], ['/missing', 404], ['/502', 502]] as const) {
    const response = await request.get(`${origin}${path}`);
    expect(response.status()).toBe(status);
    const csp = response.headers()['content-security-policy'] ?? '';
    for (const directive of ["default-src 'self'", "base-uri 'none'", "object-src 'none'", "frame-ancestors 'none'", "form-action 'self'", "script-src 'self'", "style-src 'self'"]) {
      expect(csp).toContain(directive);
    }
    expect(csp).not.toMatch(/unsafe-inline|unsafe-eval|\*|data:|https?:/);
  }
  await page.addInitScript(() => {
    const violations: string[] = [];
    (globalThis as unknown as { cspViolations: string[] }).cspViolations = violations;
    document.addEventListener('securitypolicyviolation', event => violations.push(`${event.violatedDirective} ${event.blockedURI}`));
  });
  for (const url of [`${origin}/products/canvas-tote`, 'http://127.0.0.1:4322/']) {
    await page.goto(url);
    await page.evaluate(async () => { await document.fonts.ready; });
    expect(await page.evaluate(() => (globalThis as unknown as { cspViolations: string[] }).cspViolations)).toEqual([]);
  }
  // The React retry island still hydrates under the policy.
  await expect(page.getByRole('button', { name: 'Try again' })).toBeEnabled();
});

test('storefront maximum-size published prices wrap instead of overflowing', async ({ page }) => {
  // 38-digit minor units are valid release data; the formatted price must reflow on narrow screens.
  const longest = `$${'999,'.repeat(12)}999.99`;
  for (const path of ['/products', '/products/canvas-tote']) {
    await page.goto(`${origin}${path}`);
    await page.locator('.product-price').first().evaluate((element, text) => { element.textContent = text; }, longest);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  }
});

test('storefront long published headings wrap instead of overflowing', async ({ page }) => {
  // Review ruH5c: heading blocks (10,000 chars) and product names (300) can be one long token.
  const token = 'x'.repeat(300);
  for (const [path, selector] of [['/', 'main h2'], ['/products', '.product-list h2 a']] as const) {
    await page.goto(`${origin}${path}`);
    await page.locator(selector).first().evaluate((element, text) => { element.textContent = text; }, token);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  }
});
