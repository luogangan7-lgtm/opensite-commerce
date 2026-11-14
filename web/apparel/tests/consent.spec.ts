/** Consent gate against built pages; Google and RFQ are stubbed, no external writes. */
import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
const csp = readFileSync(new URL('../dist/_headers', import.meta.url), 'utf8').match(/Content-Security-Policy: (.*)/)![1];
const enabled = !!process.env.PUBLIC_GA4_ID;
async function commands(page: import('@playwright/test').Page) {
  return page.evaluate(() => Array.from((window as any).dataLayer || [], (row: any) => Array.from(row)));
}
test.beforeEach(async ({ page }) => {
  // Synthetic widget transport only; never contacts or provisions Cloudflare.
  await page.route('https://challenges.cloudflare.com/**', route => route.fulfill({ body: '', contentType: 'text/javascript' }));
  await page.route('**/*', async route => {
    if (route.request().resourceType() !== 'document') return route.fallback();
    const response = await route.fetch();
    await route.fulfill({ response, headers: { ...response.headers(), 'content-security-policy': csp } });
  });
  await page.route('https://www.googletagmanager.com/**', route => route.fulfill({ body: '', contentType: 'text/javascript' }));
});
test('default denied, accept, persistence and privacy revocation', async ({ page }) => {
  await page.goto('/contact/');
  await expect(page.locator('[data-cookie-banner]')).toBeVisible();
  expect(await page.locator('script[src*="googletagmanager"]').count()).toBe(0);
  const defaults = (await commands(page)).filter(row => row[0] === 'consent');
  expect(defaults.length).toBe(enabled ? 1 : 0);
  if (enabled) expect(defaults[0][2]).toEqual({ analytics_storage: 'denied', ad_storage: 'denied', ad_user_data: 'denied', ad_personalization: 'denied' });
  await page.locator('[data-cookie-choice="all"]').click();
  await expect(page.locator('[data-cookie-banner]')).toBeHidden();
  if (enabled) {
    expect((await commands(page)).find(row => row[1] === 'update')?.[2]).toEqual({ analytics_storage: 'granted', ad_storage: 'granted', ad_user_data: 'granted', ad_personalization: 'granted' });
    await expect(page.locator('script[src*="googletagmanager"]')).toHaveCount(1);
  }
  await page.goto('/privacy/#cookies');
  await expect(page.locator('[data-cookie-banner]')).toBeVisible();
  await page.locator('[data-cookie-choice="essential"]').click();
  expect(await page.evaluate(() => localStorage.getItem('sourcyo-cookie-consent-v1'))).toBe('essential');
  if (enabled) expect((await commands(page)).filter(row => row[1] === 'update').at(-1)?.[2]).toEqual({ analytics_storage: 'denied', ad_storage: 'denied', ad_user_data: 'denied', ad_personalization: 'denied' });
  await page.reload();
  expect(await page.locator('script[src*="googletagmanager"]').count()).toBe(0);
});
for (const choice of ['essential', 'all']) test(`RFQ success lead only when consent: ${choice}`, async ({ page }) => {
  await page.route('**/api/rfq', route => route.fulfill({ json: { ok: true } }));
  await page.goto('/contact/?email=private@example.invalid');
  await page.locator(`[data-cookie-choice="${choice}"]`).click();
  await page.locator('[name=name]').fill('Private Name');
  await page.locator('[name=email]').fill('private@example.invalid');
  await page.locator('[name=company]').fill('Private Company');
  await page.locator('[name=message]').fill('Private inquiry');
  // Observe the real successful submit before the normal redirect clears dataLayer.
  await page.evaluate(() => window.addEventListener('apparel:lead', () => { sessionStorage.setItem('leadCommands', JSON.stringify(Array.from((window as any).dataLayer || [], (row: any) => Array.from(row)))); }));
  await page.locator('#rfq-form button[type=submit]').click();
  await page.waitForURL('**/contact/?sent=1');
  const recorded = await page.evaluate(() => JSON.parse(sessionStorage.getItem('leadCommands')!));
  expect(recorded.filter((row: any) => row[0] === 'event' && row[1] === 'generate_lead')).toHaveLength(enabled && choice === 'all' ? 1 : 0);
  expect(JSON.stringify(recorded)).not.toMatch(/private@|Private Name|Private Company|Private inquiry/);
});
test('eight locales, RTL, touch targets and unobstructed submit', async ({ page }) => {
  for (const locale of ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar']) {
    await page.goto(`${locale === 'en' ? '' : '/' + locale}/contact/`);
    const banner = page.locator('[data-cookie-banner]');
    await expect(banner).toBeVisible();
    for (const button of await banner.locator('button').all()) expect((await button.boundingBox())!.height).toBeGreaterThanOrEqual(44);
    const submit = page.locator('#rfq-form button[type=submit]');
    await submit.scrollIntoViewIfNeeded();
    await submit.click({ trial: true });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    if (locale === 'ar') await expect(page.locator('html')).toHaveAttribute('dir', 'rtl');
  }
});

if (process.env.PUBLIC_TURNSTILE_SITE_KEY) test('necessary challenge loads before and after rejecting optional analytics', async ({ page }) => {
  let scripts = 0, frames = 0;
  await page.route('https://challenges.cloudflare.com/**', route => {
    if (route.request().resourceType() === 'script') {
      scripts++;
      return route.fulfill({ contentType: 'text/javascript', body: "document.querySelector('.cf-turnstile').append(Object.assign(document.createElement('iframe'), {src: 'https://challenges.cloudflare.com/ci-frame'}));" });
    }
    frames++;
    return route.fulfill({ contentType: 'text/html', body: '<p>Synthetic necessary challenge</p>' });
  });
  await page.goto('/contact/');
  await expect(page.locator('[data-cookie-banner]')).toBeVisible();
  await expect.poll(() => scripts).toBe(1);
  await expect.poll(() => frames).toBe(1);
  await page.locator('[data-cookie-choice="essential"]').click();
  await expect(page.locator('.cf-turnstile iframe')).toBeVisible();
  expect(await page.locator('script[src*="googletagmanager"]').count()).toBe(0);
  expect(await page.evaluate(() => localStorage.getItem('sourcyo-cookie-consent-v1'))).toBe('essential');
});

test('privacy cookie settings button reopens consent in all eight locales', async ({ page }) => {
  for (const locale of ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar']) {
    await page.goto(`${locale === 'en' ? '' : '/' + locale}/privacy/`);
    const banner = page.locator('[data-cookie-banner]');
    if (await banner.isVisible()) await page.locator('[data-cookie-choice="essential"]').click();
    await expect(banner).toBeHidden();
    const settings = page.locator('#cookies [data-cookie-reopen]');
    await expect(settings).toBeVisible();
    await settings.click();
    await expect(banner).toBeVisible();
    await page.locator('[data-cookie-choice="essential"]').click();
    await expect(banner).toBeHidden();
  }
});
