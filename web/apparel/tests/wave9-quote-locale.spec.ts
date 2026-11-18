/** Scan C2 buyer copy, including interactive states and input placeholders. */
import { expect, test, type Locator } from '@playwright/test';

async function expectLocalized(scope: Locator) {
  const text = await scope.innerText();
  const placeholders = await scope.locator('[placeholder]:visible').evaluateAll(elements => elements.map(element => element.getAttribute('placeholder')));
  const unapproved = [text, ...placeholders].join(' ')
    .replace(/\b(?:[\w.+-]+@)?(?:[a-z0-9-]+\.)+[a-z]{2,}\b/gi, '') // Internet addresses
    .replace(/\b(?:MOQ|EXW|FOB|OEM|ODM|Logo|GSM|Pantone|DTG|DTF|WhatsApp|Cookie|GA4|Google|user-agent|Consent Mode|kg|cm|mm)\b/g, '')
    .match(/[A-Za-z]+/g);
  expect(unapproved ?? []).toEqual([]);
}

const footer = {
  ar: 'صور المنتجات توضيحية، وصور المصنع حقيقية.',
  zh: '产品图为示意图，工厂照片为实拍。',
};

for (const locale of ['ar', 'zh'] as const) {
  test(`F3 C2 localized copy and FAQ ${locale}`, async ({ page }) => {
    await page.goto(`/${locale}/`);
    await expectLocalized(page.locator('.home-closing'));

    await page.goto(`/${locale}/contact/`);
    const main = page.locator('main');
    await expectLocalized(main);
    await expect(page.locator('.footer-legal')).toContainText(footer[locale]);
    const questions = main.locator('details[name="contact-faq"]');
    await expect(questions).toHaveCount(3);
    for (const question of await questions.all()) {
      await question.locator('summary').click();
      await expect(question.locator('p')).toBeVisible();
      await expect(main.locator('details[open]')).toHaveCount(1);
      await expectLocalized(question);
    }
  });
}
