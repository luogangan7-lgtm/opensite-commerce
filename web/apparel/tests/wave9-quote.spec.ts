/** F3 RFQ layout gate. CI checks geometry before external images are packaged. */
import { expect, test, type Locator } from '@playwright/test';
import en from '../src/i18n/en.json' with { type: 'json' };
import ar from '../src/i18n/ar.json' with { type: 'json' };
import zh from '../src/i18n/zh.json' with { type: 'json' };

const dictionaries = { en, ar, zh };
const quoteTerms = { en: 'Confirmed in quote', ar: 'يُؤكَّد في عرض السعر', zh: '以报价单为准' };

async function box(locator: Locator) {
  await expect(locator).toBeVisible();
  return (await locator.boundingBox())!;
}

const mobileContactHeight = { en: 4015, ar: 3783, zh: 3772 };
for (const locale of ['en', 'ar', 'zh'] as const) for (const width of [390, 768, 1440]) {
  test(`F3 quote sibling rows ${locale} ${width}`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    for (const route of ['/', '/contact/']) {
      await page.goto(`${locale === 'en' ? '' : '/' + locale}${route}`);
      await page.evaluate(() => document.fonts.ready);
      const row = page.locator(route === '/' ? '.home-closing' : '.contact-form');
      await expect(row.locator('h2')).toHaveCSS('font-weight', '400');
      if (route === '/contact/') {
        const label = await page.locator('.quote-note').innerText();
        expect(label.normalize()).toBe(quoteTerms[locale].normalize());
      }
      const photo = await box(row.locator('[data-contact-media=factory]'));
      const copy = await box(row.locator('.rfq-contact-links'));
      const card = await box(row.locator('.rfq'));
      const intro = await box(row.locator('.rfq-intro'));
      const field = await box(row.locator('[name=message]'));
      const submit = row.getByRole('button', { name: dictionaries[locale].rfqSend });
      const cta = await box(submit);
      expect(Math.abs(field.x - cta.x)).toBeLessThanOrEqual(2);
      expect(Math.abs(field.width - cta.width)).toBeLessThanOrEqual(2);
      expect(cta.x).toBeGreaterThanOrEqual(0);
      expect(cta.x + cta.width).toBeLessThanOrEqual(width);
      expect(intro.y).toBeGreaterThan(card.y);
      expect(cta.y + cta.height).toBeLessThan(card.y + card.height);
      if (width > 1023) {
        expect(Math.abs(photo.y - card.y)).toBeLessThanOrEqual(2);
        expect(Math.abs(photo.x + (locale === 'ar' ? photo.width : 0) - copy.x - (locale === 'ar' ? copy.width : 0))).toBeLessThanOrEqual(2);
      } else if (route === '/contact/') {
        expect(Math.abs(photo.y + photo.height / 2 - copy.y - copy.height / 2)).toBeLessThanOrEqual(2);
      } else {
        expect(Math.abs(photo.x - card.x)).toBeLessThanOrEqual(2);
        expect(Math.abs(photo.width - card.width)).toBeLessThanOrEqual(2);
      }
      if (route === '/contact/' && width === 390) {
        const height = await page.evaluate(() => document.documentElement.scrollHeight);
        expect(Math.abs(height / mobileContactHeight[locale] - 1)).toBeLessThanOrEqual(0.1);
      }
      if (route === '/contact/') {
        const steps = await page.locator('.next-grid li').all();
        expect(steps).toHaveLength(3);
        const numbers = await Promise.all(steps.map(step => box(step.locator('span'))));
        const headings = await Promise.all(steps.map(step => box(step.locator('h3'))));
        for (const boxes of [numbers, headings]) {
          const positions = boxes.map(item => width > 1023 ? item.y : item.x + (locale === 'ar' ? item.width : 0));
          expect(Math.max(...positions) - Math.min(...positions)).toBeLessThanOrEqual(2);
        }
      }
      if (route === '/') {
        const form = row.locator('.rfq');
        await expect(form.locator('[name=quantity_note]')).toHaveAttribute('placeholder', dictionaries[locale].quantityPlaceholder);
        await page.route('**/api/rfq', request => request.fulfill({ status: 503, json: {} }));
        await submit.click();
        await expect(form).toHaveAttribute('data-state', 'Error');
        await expect(form.locator('[name=email]')).toHaveAttribute('aria-invalid', 'true');
        await expect(form.locator('#rfq-error-email')).toBeVisible();
        const error = await form.evaluate(node => {
          const color = getComputedStyle(node).getPropertyValue('--sy-color-error').trim();
          const canvas = document.createElement('canvas').getContext('2d')!;
          canvas.fillStyle = color;
          return canvas.fillStyle;
        });
        const statusMatchesError = () => form.locator('.form-status').evaluate((node, color) => {
          const canvas = document.createElement('canvas').getContext('2d')!;
          canvas.fillStyle = getComputedStyle(node).color;
          return canvas.fillStyle === color;
        }, error);
        expect(await statusMatchesError()).toBe(true);
        for (const [name, value] of Object.entries({ first_name: 'Test', last_name: 'Buyer', email: 'buyer@example.invalid', company: 'Test buyer', quantity_note: '42', message: 'Test' })) {
          await form.locator(`[name=${name}]`).fill(value);
        }
        await submit.click();
        await expect(form).toHaveAttribute('data-state', 'ServerError');
        await expect(form.locator('.form-status')).toHaveText(dictionaries[locale].rfqServerError);
        expect(await statusMatchesError()).toBe(true);
      }
    }
  });
}
