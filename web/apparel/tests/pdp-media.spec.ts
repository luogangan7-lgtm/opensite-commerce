/** CP4 rendered-media regression against frozen FD1/FD3/FD5. */
import { test, expect } from '@playwright/test';
import products from '../src/data/products.json' with { type: 'json' };
import { productText, dictionaries as translations } from '../src/i18n';
const dictionaries = Object.fromEntries(['en','zh','es','fr','de','pt','ru','ar'].map(locale=>[locale,{}]));
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
 await expect(page.locator('[data-pdp-media] figcaption')).toHaveCount(0);
 const language = locale as keyof typeof translations;
 const detailAlt = await page.locator('[data-pdp-detail]').getAttribute('alt');
 expect(detailAlt).toContain(productText[language]['hoodie-set-k02'].name);
 expect(detailAlt).toContain(translations[language].view);
});
for(const locale of ['en','ar','zh']) for(const width of [390,768,1280]) test(`FIT pair and viewer ${locale} ${width}`,async({page})=>{
 await page.setViewportSize({width,height:1000});
 await page.goto(path(locale));
 const pair=page.locator('[data-pdp-media]'); await pair.scrollIntoViewIfNeeded();
 const whole=pair.locator('[data-pdp-whole]'),detail=pair.locator('[data-pdp-detail]');
 for(const image of [whole,detail]) {
  await image.evaluate(i=>i.scrollIntoView({behavior:'instant',block:'center'}));
  await expect(image).toHaveCSS('object-fit','contain');
  // CI has no external masters; local reconstruction is audit-only (APPAREL_PDP_IMAGES=1).
  if (process.env.APPAREL_PDP_IMAGES==='1') await expect.poll(()=>image.evaluate((i:HTMLImageElement)=>i.complete && i.naturalWidth>0)).toBe(true);
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
for(const locale of Object.keys(dictionaries)) test(`RFQ selected colour contract ${locale}`,async({page})=>{
 await page.goto(path(locale));
 const swatches=page.locator('[data-gallery-swatch]');
 for(const swatch of [swatches.first(),swatches.nth(1),swatches.first()]) {
  await swatch.click();
  const colour=await swatch.getAttribute('data-gallery-swatch');
  for(const [index,type] of ['quote','sample'].entries()) {
   const href=await page.locator('.product-info .actions a').nth(index).getAttribute('href');
   const url=new URL(href!,'https://fixture.test');
   expect(url.pathname).toBe(`${locale==='en'?'':'/'+locale}/contact/`);
   expect(url.searchParams.get('product')).toBe('hoodie-set-k02');
   expect(url.searchParams.get('type')).toBe(type);
   expect(url.searchParams.get('colour')).toBe(colour);
   expect(url.hash).toBe('#rfq-form');
  }
 }
});

test('PDP swatch Quote click prefills the delivered RFQ form',async({page})=>{
 await page.goto(path('en'));
 const swatch=page.locator('[data-gallery-swatch]').nth(1);
 await swatch.click();
 const colour=await swatch.getAttribute('data-gallery-swatch');
 await page.locator('[data-rfq-intent="quote"]').click();
 await expect(page).toHaveURL(/contact\/\?product=hoodie-set-k02&type=quote&colour=/);
 const form=page.locator('form.rfq');
 await expect(form.locator('[name=product_slug]')).toHaveValue('hoodie-set-k02');
 await expect(form.locator('[data-prefill-context]')).toContainText(colour!);
 await expect(form.locator('textarea')).toHaveValue(new RegExp(colour!));
 await expect(form.locator('[name=quantity_note]')).toHaveValue('');
});
