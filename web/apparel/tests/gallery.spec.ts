/** Browser regression for §6.2/6.3: served PDP interaction, disclosure and uncropped details. */
import { test, expect } from '@playwright/test';
const captions = {
 en: 'AI-generated illustration; actual details are subject to the confirmed sample.',
 zh: 'AI生成示意，实际细节以确认样品为准',
 es: 'Ilustración generada por IA; los detalles reales dependen de la muestra confirmada.',
 fr: 'Illustration générée par IA ; les détails réels sont ceux de l’échantillon confirmé.',
 de: 'KI-generierte Illustration; maßgeblich sind die tatsächlichen Details des bestätigten Musters.',
 pt: 'Ilustração gerada por IA; os detalhes reais dependem da amostra confirmada.',
 ru: 'Иллюстрация создана ИИ; фактические детали определяются подтверждённым образцом.',
 ar: 'رسم توضيحي مولّد بالذكاء الاصطناعي؛ التفاصيل الفعلية وفق العينة المعتمدة.',
};
for (const [locale, caption] of Object.entries(captions)) test(`gallery details ${locale}`, async ({ page }) => {
 await page.goto(`${locale === 'en' ? '' : '/' + locale}/products/crew-fleece-8900-31/`);
 const links = page.locator('.gallery-thumbs a');
 const roles = await links.evaluateAll(nodes => nodes.map(n => (n as HTMLElement).dataset.role));
 const rank: Record<string, number> = {front:0,turn:1,back:2,print:3,garment:4,structure:5,fabric:6};
 const known = roles.filter(r => r && r in rank).map(r => rank[r!]);
 expect(known).toEqual([...known].sort((a,b)=>a-b));
 const detail = page.locator('.gallery-thumbs a[data-role="fabric"]');
 await expect(detail).toHaveCount(1);
 await detail.click();
 const main = page.locator('[data-gallery-main]');
 await expect(main).toHaveCSS('object-fit', 'contain');
 if (process.env.APPAREL_PACKAGED_IMAGES === '1' || process.env.APPAREL_PREVIEW_URL) await expect.poll(() => main.evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth === 1200 && img.naturalHeight === 1200)).toBe(true);
 const box = await main.boundingBox(); expect(box!.width / box!.height).toBeCloseTo(4/5, 2);
 await expect(page.locator('[data-digital-label]')).toHaveText(caption);
 await expect(main).toHaveAttribute('alt', caption);
 await page.locator('[data-gallery-zoom]').click();
 await expect(page.locator('dialog')).toBeVisible();
 await expect(page.locator('dialog')).toHaveAttribute('aria-label', caption);
 await expect(page.locator('dialog img')).toHaveCSS('object-fit', 'contain');
 await page.keyboard.press('Escape');
 await links.first().click();
 await expect(page.locator('[data-digital-label]')).not.toHaveText(caption);
});
