/** C9 browser gate: approved bytes must reach served PDP slots, including colour selection. */
import { test, expect } from '@playwright/test';
import { createHash } from 'node:crypto';
import accepted from './accepted-regenerated.json' with { type: 'json' };
import products from '../src/data/products.json' with { type: 'json' };
test('63 independently accepted regenerated slots are served and selectable', async ({page, request}) => {
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
 expect(manifest.digital).toHaveLength(739);
 for(const row of accepted) {
  const receipt = manifest.digital.find((r: {src:string}) => r.src === row.src);
  expect(receipt?.sha256, row.exactId).toBe(row.sha256);
  const response = await request.get(row.src);
  expect(response.ok(),row.src).toBe(true);
  expect(createHash('sha256').update(await response.body()).digest('hex')).toBe(manifest.files.find((f: {url:string})=>f.url===row.src).sha256);
 }
 }
});
