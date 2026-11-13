/** FIN accepted media: five primary structures and the final missing beige front. */
import { test, expect } from '@playwright/test';
for(const slug of ['apparel-70194','pullover-hoodie-69010','pullover-hoodie-69012','pullover-hoodie-69014','pullover-hoodie-69015']) test(`FIN structure ${slug}`,async({page})=>{
 await page.goto(`/zh/products/${slug}/`);
 const link=page.locator('.gallery-thumbs:not([hidden]) a[data-role="structure"]');
 await expect(link).toHaveCount(1); await link.click();
 const main=page.locator('[data-gallery-main]'),caption='AI生成示意，实际细节以确认样品为准';
 await expect(main).toHaveAttribute('src',`/img/${slug}/structure.webp`);
 await expect(main).toHaveCSS('object-fit','contain');
 await expect(page.locator('[data-digital-label]')).toHaveText(caption);
 if(process.env.APPAREL_FIN_IMAGES==='1') await expect.poll(()=>main.evaluate((i:HTMLImageElement)=>i.complete&&i.naturalWidth===1254&&i.naturalHeight===1254)).toBe(true);
 await page.locator('[data-gallery-zoom]').click();
 await expect(page.locator('dialog')).toHaveAttribute('aria-label',caption);
 await expect(page.locator('dialog img')).toHaveCSS('object-fit','contain');
});
test('FIN 70179 beige/sand swatch selects accepted model-03 front',async({page})=>{
 await page.goto('/products/apparel-70179/');
 const swatch=page.locator('[data-gallery-swatch="beige/sand"]');
 await expect(swatch).toHaveCount(1); await swatch.click();
 const main=page.locator('[data-gallery-main]');
 await expect(main).toHaveAttribute('src','/img/apparel-70179/digital-model-03.webp');
 await expect(main).toHaveAttribute('alt',/beige/);
 if(process.env.APPAREL_FIN_IMAGES==='1') await expect.poll(()=>main.evaluate((i:HTMLImageElement)=>i.complete&&i.naturalWidth===1024&&i.naturalHeight===1280)).toBe(true);
});
