/** C12 independent PASS fixture binds every placed style; private opt-in verifies raw native pixels. */
import { test, expect } from 'bun:test';
import accepted from './accepted-structure.json';
import details from '../detail-sources.json';
import ledger from '../structure-ledger.json';
import products from '../src/data/products.json';
import { orderedGallery, primaryColour, detailCaption } from '../src/data/gallery';
import { imagePlan, imageDimensions, type DetailSource } from '../image-plan';
import { packageDetails } from '../detail-images';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
test('108 independently accepted structures bind primary galleries; #552 colour conflicts stay excluded',()=>{
 expect(accepted).toHaveLength(110);
 const placed=accepted.filter(r=>!r.skipped);
 const structures=details.filter(r=>r.role==='structure');
 expect(placed).toHaveLength(108); expect(structures).toHaveLength(108); expect(ledger).toHaveLength(108);
 const plan=imagePlan({slots:{},review:[],provenance:{},published:products.map(p=>p.slug),details:structures as DetailSource[],detailLedger:ledger});
 for(const row of placed) {
  const source=plan.details.find(i=>i.slug===row.slug)!;
  expect(source.sha256,row.exactId).toBe(row.sha256);
  const product=products.find(p=>p.slug===row.slug)!;
  expect(source.colour).toBe(primaryColour(product.images));
  const gallery=orderedGallery(product.images), image=gallery.find(i=>i.role==='structure');
  expect(image?.src).toBe(source.src); expect(image?.sha256_16).toBe(row.sha256.slice(0,16));
  if(gallery.some(i=>i.role==='fabric')) expect(gallery.indexOf(image!)).toBeLessThan(gallery.findIndex(i=>i.role==='fabric'));
 }
 for(const slug of ['crew-fleece-8900-53','apparel-70190']) expect(structures.some(i=>i.slug===slug)).toBe(false);
 expect(Object.values(detailCaption)).toHaveLength(8);
});
test.skipIf(!process.env.APPAREL_SOURCE_DIR)('108 raw hashes and independent reviews package as native square structures', async()=>{
 const root=process.env.APPAREL_SOURCE_DIR!;
 const rows=details.filter(r=>r.role==='structure');
 for(const row of accepted.filter(r=>!r.skipped)) {
  const image=rows.find(i=>i.slug===row.slug)!;
  expect(createHash('sha256').update(readFileSync(join(root,image.source))).digest('hex')).toBe(row.sha256);
  expect(readFileSync(join(root,image.acceptance),'utf8')).toContain(`${row.exactId}\t${row.sha256}\tPASS`);
  const html=readFileSync(`web/apparel/dist/zh/products/${row.slug}/index.html`,'utf8');
  const link=html.match(new RegExp(`<a href="${image.src}"[^>]*>`))?.[0];
  expect(link).toContain('data-role="structure"');
  expect(link).toContain(`data-digital="${detailCaption.zh}"`);
  expect(link).toContain('data-fit="contain"');
 }
 const receipts=await packageDetails(rows as DetailSource[],root,'web/apparel/dist');
 expect(receipts).toHaveLength(108);
 for(const r of receipts) expect([r.w,r.h]).toEqual([1254,1254]);
}, 30000);

test('native structure minimum accepts 1254 square but rejects undersized and non-square sources',()=>{
 expect(imageDimensions('structure',{width:1254,height:1254})).toEqual({w:1254,h:1254});
 for(const [width,height] of [[1199,1199],[1254,1200],[1200,1254]]) expect(()=>imageDimensions('structure',{width,height})).toThrow(/1200/);
});
