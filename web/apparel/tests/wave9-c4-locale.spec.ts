/** Guard buyer-visible AR/ZH shell and editorial text against English leakage. */
import { expect, test } from '@playwright/test';

const routes = ['oem-odm', 'about-us', 'sampling', 'shipping', '404', 'terms', 'privacy'];
const allowances = /\b(?:MOQ|EXW|FOB|OEM|ODM|Logo|GSM|Pantone|DTG|DTF|WhatsApp|Cookie|GA4|Google|user-agent|Consent Mode|kg|cm|mm|S|M|L|X{1,5}L|[2-5]XL)\b/g;

for (const locale of ['ar', 'zh']) for (const route of routes) {
  test(`C4 Latin allowlist · ${locale}/${route}`, async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 844 });
    await page.goto(`/${locale}/${route}/`);
    const visible = (await page.locator('main, footer, .header-inner > nav, .header-inner > .button').allInnerTexts()).join(' ');
    const unapproved = visible.normalize('NFKC')
      .replace(/\b(?:[\w.+-]+@)?(?:[a-z0-9-]+\.)+[a-z]{2,}\b/gi, '')
      .replace(/\b(?=[A-Z0-9-]*\d)[A-Z][A-Z0-9-]*\b/g, '')
      .replace(/\b\d[0-9A-Z]*[A-Z][0-9A-Z]*\b/g, '')
      .replace(allowances, '')
      .match(/[A-Za-z]+/g);
    expect([...new Set(unapproved ?? [])]).toEqual([]);
  });
}
