/** C3 localized content, canonical weight units and sticky mobile/tablet Size column. */
import { expect, test } from '@playwright/test';

const allowed = /\b(?:MOQ|EXW|FOB|OEM|ODM|Logo|GSM|Pantone|DTG|DTF|WhatsApp|Cookie|GA4|Google|user-agent|Consent Mode|cm|kg)\b/gi;

for (const locale of ['ar', 'zh']) {
  test(`${locale} Home and PDP have localized visible copy`, async ({ page }) => {
    for (const route of ['/', '/products/cotton430-hoodie-58065/']) {
      await page.goto('/' + locale + route);
      const content = await page.locator('main').innerText();
      const latin = content
        // F3 uses the wordmark in the benefits heading and T as part of the garment name.
        .replace(/(?:‏لماذا|为什么选择) Sourcyo|T[\s\u00a0]*恤/giu, '')
        .replace(/[\w.+-]+@sourcyowear\.com|sourcyowear\.com/giu, '')
        .replace(/\b[A-Z]*\d+[A-Z\d]*(?:-\d+)*\b|\b(?:XXS|XS|S|M|L|XL)\b/g, '')
        .replace(allowed, '').match(/[a-z]+/gi) ?? [];
      expect.soft(latin, route).toEqual([]);
    }
  });
  for (const width of [390, 768]) test(`${locale} ${width} canonical size chart`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto('/' + locale + '/products/cotton430-hoodie-58065/');
    const chart = page.locator('#size-chart');
    await expect(chart.locator('p bdi')).toHaveText('±1–2');
    await expect(chart.locator('tbody tr').first().locator('[data-measurement=weight_jin]'))
      .toHaveText(locale === 'ar' ? '50-58 كغ' : '100-115斤');
    await expect(chart.locator('tbody th').first()).toHaveText('M');
    await expect(chart.locator('thead th').first()).toHaveCSS('position', 'sticky');
    await expect(chart.getByRole('region')).toHaveCSS('overflow-x', 'scroll');
  });
}
