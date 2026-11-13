/** C9 + R4 acceptance pool is an independent fixture; no unaccepted slot may change. */
import { test, expect } from 'bun:test';
import accepted from './accepted-regenerated.json';
import digital from '../digital-sources.json';
import products from '../src/data/products.json';
import metadata from '../src/data/digital-imagery.json';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import { orderedGallery } from '../src/data/gallery';
import { imagePlan } from '../image-plan';
test('all 78 accepted source SHAs are bound to existing packaging and gallery slots',()=>{
 expect(accepted).toHaveLength(78);
 expect(new Set(accepted.map(r=>r.src)).size).toBe(78);
 const plan=imagePlan({slots:{},review:[],provenance:{},published:products.map(p=>p.slug),digital});
 for(const row of accepted) {
  const source=plan.digital.find(r=>r.src===row.src);
  expect(source?.sha256,row.exactId).toBe(row.sha256);
  expect(source?.source).toBe(row.source);
  const image=products.find(p=>p.slug===row.slug)!.images.find(i=>i.src===row.src);
  expect(image?.sha256_16).toBe(row.sha256.slice(0,16));
  expect(image?.role).toBe(row.view);
  expect(orderedGallery(products.find(p=>p.slug===row.slug)!.images,image?.colour).some(i=>i.src===row.src)).toBe(true);
  expect(metadata.find(i=>i.src===row.src)?.view).toBe(row.view);
 }
});

test.skipIf(!process.env.APPAREL_SOURCE_DIR)('private 78 originals match independent PASS rows and source digests',()=>{
 for(const row of accepted) {
  const bytes=readFileSync(join(process.env.APPAREL_SOURCE_DIR!,row.source));
  expect(createHash('sha256').update(bytes).digest('hex')).toBe(row.sha256);
  expect(readFileSync(join(process.env.APPAREL_SOURCE_DIR!,row.acceptance),'utf8')).toContain(`${row.exactId}\t${row.sha256}\tPASS`);
 }
});
