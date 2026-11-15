/** C12: representative accepted structures use the existing localized disclosure and contain viewer. */
import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
const caption = JSON.parse(readFileSync(new URL('../src/i18n/zh.json', import.meta.url), 'utf8')).pdpDetailCaption;
for (const slug of ['crew-fleece-8900-31','apparel-59043','pullover-hoodie-69008']) test(`accepted structure ${slug}`, async ({page}) => {
 await page.goto(`/zh/products/${slug}/`);
 const link=page.locator('.gallery-thumbs:not([hidden]) a[data-role="structure"]');
 await expect(link).toHaveCount(1);
 await link.click();
 const main=page.locator('[data-gallery-main]');
 await expect(main).toHaveAttribute('src',`/img/${slug}/structure.webp`);
 await expect(main).toHaveCSS('object-fit','contain');
 if (process.env.APPAREL_STRUCTURE_IMAGES==='1') await expect.poll(()=>main.evaluate((img:HTMLImageElement)=>img.complete && img.naturalWidth===1254 && img.naturalHeight===1254)).toBe(true);
 const box=await main.boundingBox(); expect(box!.width/box!.height).toBeCloseTo(4/5,2);
 await expect(page.locator('[data-digital-label]')).toHaveText(caption);
 await expect(main).toHaveAttribute('alt',caption);
 await page.locator('[data-gallery-zoom]').click();
 await expect(page.locator('dialog')).toBeVisible();
 await expect(page.locator('dialog')).toHaveAttribute('aria-label',caption);
 await expect(page.locator('dialog img')).toHaveCSS('object-fit','contain');
});
