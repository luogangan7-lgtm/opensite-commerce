/** Guard buyer-visible AR/ZH shell and editorial text against English leakage. */
import { expect, test } from '@playwright/test';

const routes = ['oem-odm', 'about-us', 'sampling', 'shipping', '404', 'terms', 'privacy'];
const allowances = /\b(?:MOQ|EXW|FOB|OEM|ODM|Logo|GSM|Pantone|DTG|DTF|WhatsApp|Cookie|GA4|Google|user-agent|Consent Mode|kg|cm|mm|S|M|L|X{1,5}L|[2-5]XL)\b/g;

// Exact visible F3 literals approved in Orca msg_b2e814e040b3.
const brand = /\bSourcyo Apparel\b/gi; // Brand, including the About uppercase eyebrow
const company = /\bHunan Xuanliang Eco-Technology Co\., Ltd\./g; // Contracting entity
const aboutNames = [
  /\bYidian Apparel\b/g, // Wholesale stall
  /\bJindong International Garment City\b/g, // Wholesale site
];
const privacyTerms = [
  /\bSupabase\b/g, // Data processor
  /\bGoogle Analytics\b/g, // Analytics service
  /\bGoogle LLC\b/g, // Analytics provider
  /\bConsent Mode v2\b/g, // Consent configuration version
  /\banalytics_storage\b/g, // Analytics consent setting
  /\bad_storage\b/g, // Advertising storage consent setting
  /\bad_user_data\b/g, // Advertising data consent setting
  /\bad_personalization\b/g, // Advertising personalization consent setting
  /\bdenied\b/g, // Default setting value
];

for (const locale of ['ar', 'zh']) for (const route of routes) {
  test(`C4 Latin allowlist · ${locale}/${route}`, async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 844 });
    await page.goto(`/${locale}/${route}/`);
    let visible = (await page.locator('main, footer, .header-inner > nav, .header-inner > .button').allInnerTexts()).join(' ').normalize('NFKC');
    visible = visible.replace(brand, '');
    if (['about-us', 'terms', 'privacy'].includes(route)) visible = visible.replace(company, '');
    if (route === 'about-us') for (const phrase of aboutNames) visible = visible.replace(phrase, '');
    if (route === 'privacy') for (const phrase of privacyTerms) visible = visible.replace(phrase, '');
    if (locale === 'zh' && ['about-us', '404'].includes(route)) {
      visible = visible.replace(/\bT恤/g, '').replace(/\bT 恤/g, ''); // Chinese garment term, both F3 spacings
    }
    const unapproved = visible
      .replace(/\b(?:[\w.+-]+@)?(?:[a-z0-9-]+\.)+[a-z]{2,}\b/gi, '')
      .replace(/\b(?=[A-Z0-9-]*\d)[A-Z][A-Z0-9-]*\b/g, '')
      .replace(/\b\d[0-9A-Z]*[A-Z][0-9A-Z]*\b/g, '')
      .replace(allowances, '')
      .match(/[A-Za-z]+/g);
    expect([...new Set(unapproved ?? [])]).toEqual([]);
  });
}
