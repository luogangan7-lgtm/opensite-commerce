/** CP4 rendered-media regression against frozen FD1/FD3/FD5. */
import { test, expect } from '@playwright/test';
import products from '../src/data/products.json' with { type: 'json' };
import { readFileSync } from 'node:fs';
const dictionaries: Record<string,{pdpDetailCaption:string}> = Object.fromEntries(['en','zh','es','fr','de','pt','ru','ar'].map(locale=>[locale,JSON.parse(readFileSync(new URL(`../src/i18n/${locale}.json`,import.meta.url),'utf8'))]));
const path=(locale:string,slug='hoodie-set-k02')=>`${locale==='en'?'':'/'+locale}/products/${slug}/`;
test('all 115 served PDPs use their own primary whole and same-colour structure', async ({page}) => {
 test.setTimeout(120000);
 for (const product of products) {
  await page.goto(path('en',product.slug));
  const whole=product.images.find(i=>i.role==='front')!;
  const detail=product.images.find(i=>i.role==='structure' && i.colour===whole.colour)!;
  await expect(page.locator('[data-pdp-whole]')).toHaveAttribute('src',whole.src);
  await expect(page.locator('[data-pdp-detail]')).toHaveAttribute('src',detail.src);
 }
});
for(const [locale,t] of Object.entries(dictionaries)) test(`caption ${locale}`,async({page})=>{
 await page.goto(path(locale));
 await expect(page.locator('[data-pdp-media] figcaption')).toHaveText(t.pdpDetailCaption);
});
for(const locale of ['en','ar','zh']) for(const width of [390,768,1280]) test(`FIT pair and viewer ${locale} ${width}`,async({page})=>{
 await page.setViewportSize({width,height:1000});
 await page.goto(path(locale));
 const pair=page.locator('[data-pdp-media]'); await pair.scrollIntoViewIfNeeded();
 const whole=pair.locator('[data-pdp-whole]'),detail=pair.locator('[data-pdp-detail]');
 for(const image of [whole,detail]) {
  await expect(image).toHaveCSS('object-fit','contain');
  await expect.poll(()=>image.evaluate((i:HTMLImageElement)=>i.complete && i.naturalWidth>0)).toBe(true);
 }
 const a=(await whole.boundingBox())!,b=(await detail.boundingBox())!;
 expect(a.width/a.height).toBeCloseTo(4/5,2); expect(b.width/b.height).toBeCloseTo(1,2);
 if(width===390) expect(b.y).toBeGreaterThanOrEqual(a.y+a.height);
 else { expect(b.y).toBeCloseTo(a.y,0); expect(locale==='ar'?b.x<a.x:b.x>a.x).toBe(true); }
 expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
 await page.locator('[data-gallery-main]').click();
 await expect(page.locator('[data-gallery-viewer]')).toBeVisible();
 expect(await page.locator('[data-gallery-viewer] img').evaluate((i:HTMLImageElement)=>i.src)).toBe(await page.locator('[data-gallery-main]').evaluate((i:HTMLImageElement)=>i.src));
 await page.keyboard.press('Escape');
 await page.locator('.gallery-thumbs:not([hidden]) a[data-role="structure"]').click();
 await page.locator('[data-gallery-main]').click();
 expect(await page.locator('[data-gallery-viewer] img').evaluate((i:HTMLImageElement)=>i.src)).toBe(await detail.evaluate((i:HTMLImageElement)=>i.src));
 await page.keyboard.press('Escape');
 const swatches=page.locator('[data-gallery-swatch]');
 await swatches.nth(1).click(); await expect(swatches.nth(1)).toHaveAttribute('aria-pressed','true');
 await expect(page.locator('[data-gallery-main]')).toHaveAttribute('src',await page.locator('.gallery-thumbs:not([hidden]) a').first().getAttribute('href') || '');
});
