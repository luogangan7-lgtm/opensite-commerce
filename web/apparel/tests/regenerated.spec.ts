/** C9 + R4 browser gate: approved bytes must reach served PDP slots, including colour selection. */
import { test, expect } from '@playwright/test';
import { createHash } from 'node:crypto';
import accepted from './accepted-regenerated.json' with { type: 'json' };
import products from '../src/data/products.json' with { type: 'json' };
import bindings from './colour-bindings.json' with { type: 'json' };
test('78 independently accepted regenerated slots are served and selectable', async ({page, request}) => {
 test.setTimeout(180_000);
 for(const slug of new Set(accepted.map(r=>r.slug))) {
  await page.goto(`/products/${slug}/`);
  for(const row of accepted.filter(r=>r.slug===slug)) {
   const image=products.find(p=>p.slug===slug)!.images.find(i=>i.src===row.src)!;
   if(image?.colour) await page.locator(`[data-gallery-swatch="${image.colour}"]`).click();
   const link=page.locator(`.gallery-thumbs:not([hidden]) a[href="${row.src}"]`);
   await expect(link,row.exactId).toHaveCount(1);
   await link.click();
   await expect(page.locator('[data-gallery-main]')).toHaveAttribute('src',row.src);
   if(process.env.APPAREL_PACKAGED_IMAGES === '1' || process.env.APPAREL_PREVIEW_URL) await expect.poll(()=>page.locator('[data-gallery-main]').evaluate((i: HTMLImageElement)=>i.complete && i.naturalWidth>0)).toBe(true);
  }
 }
 if(process.env.APPAREL_PACKAGED_IMAGES === '1' || process.env.APPAREL_PREVIEW_URL) {
 const manifest = await (await request.get('/deploy-manifest.json')).json();
 expect(manifest.digital).toHaveLength(753);
 for(const row of accepted) {
  const receipt = manifest.digital.find((r: {src:string}) => r.src === row.src);
  expect(receipt?.sha256, row.exactId).toBe(row.sha256);
  const response = await request.get(row.src);
  expect(response.ok(),row.src).toBe(true);
  expect(createHash('sha256').update(await response.body()).digest('hex')).toBe(manifest.files.find((f: {url:string})=>f.url===row.src).sha256);
 }
 }
});

// C11: model-01 turn/back must follow the catalog-bound model-01 front, not legacy colour labels.
const reboundStyles = [
 'cotton430-hoodie-8601', 'hoodie-set-k01-0256-x-croc', 'hoodie-set-k01-0256-tz2804-8',
 'hoodie-set-k01-0256-tz2800-10', 'hoodie-set-k02', 'hoodie-set-k06',
 'hoodie-set-k12-3900-0252', 'hoodie-set-k12-3900', 'hoodie-set-k21-70046',
 'hoodie-set-k22-6000-2', 'hoodie-set-k01-0256-tz2804-7', 'hoodie-set-k01-0256',
 'hoodie-set-k04', 'hoodie-set-k05', 'hoodie-set-k23-70080',
];
for (const slug of reboundStyles) test(`C11 model-01 W3 follows catalog colour: ${slug}`, async ({ page }) => {
 const binding = bindings.find(row => row.slug === slug && row.src === `/img/${slug}/digital-front.webp`)!;
 const rows = accepted.filter(row => row.slug === slug && /model-01-(turn|back)-/.test(row.exactId));
 expect(rows.length).toBeGreaterThan(0);
 await page.goto(`/products/${slug}/`);
 const swatch = page.locator(`[data-gallery-swatch="${binding.colour}"]`);
 await expect(swatch).toHaveAttribute('aria-pressed', 'true');
 for (const row of rows) {
  const link = page.locator(`.gallery-thumbs:not([hidden]) a[href="${row.src}"]`);
  await expect(link, row.exactId).toHaveAttribute('data-colour', binding.colour);
  await link.click();
  await expect(page.locator('[data-gallery-main]')).toHaveAttribute('src', row.src);
 }
 const other = page.locator('[data-gallery-swatch]').filter({ hasNot: page.locator(`img[src="${binding.src}"]`) }).first();
 await other.click();
 for (const row of rows) await expect(page.locator(`.gallery-thumbs:not([hidden]) a[href="${row.src}"]`)).toHaveCount(0);
 await swatch.click();
 for (const row of rows) await expect(page.locator(`.gallery-thumbs:not([hidden]) a[href="${row.src}"]`)).toHaveAttribute('data-colour', binding.colour);
});
