/** C21 acceptance pool binds exact native bytes and only the designated gallery slots. */
import {test,expect} from 'bun:test';
import accepted from './accepted-c21.json';
import digital from '../digital-sources.json';
import details from '../detail-sources.json';
import products from '../src/data/products.json';
import {imageDimensions} from '../image-plan';
import {orderedGallery,detailCaption} from '../src/data/gallery';
import {readFileSync} from 'node:fs';
import {createHash} from 'node:crypto';
import {createRequire} from 'node:module';
const sharp=createRequire(import.meta.resolve('astro/package.json'))('sharp');
test('native fabric contract admits 1200 and 1254 square; rejects smaller/non-square; print stays exact',()=>{
 for(const n of [1200,1254]) expect(imageDimensions('fabric',{width:n,height:n})).toEqual({w:n,h:n});
 for(const [width,height] of [[1199,1199],[1254,1200]]) expect(()=>imageDimensions('fabric',{width,height})).toThrow();
 expect(()=>imageDimensions('print',{width:1254,height:1254})).toThrow();
});
for(const row of accepted) test(`${row.exactId}: F6-IMG slot and replacement source`,async()=>{
 const descriptor=[...digital,...details].find(d=>d.src===row.src);
 expect(descriptor?.sha256).toBe(row.sha256);expect(descriptor?.source).toBe(row.source);
 const image=orderedGallery(products.find(p=>p.slug===row.slug)!.images).find(i=>i.src===row.src);
 expect(image?.sha256_16).toBe(row.sha256.slice(0,16));expect(image?.role).toBe(row.view);
 if(row.replaced) expect(digital.some(d=>d.source===row.oldSource)).toBe(false);
 if(process.env.APPAREL_SOURCE_DIR){
 const bytes=readFileSync(`${process.env.APPAREL_SOURCE_DIR}/${row.source}`);
 expect(createHash('sha256').update(bytes).digest('hex')).toBe(row.sha256);
 const meta=await sharp(bytes).metadata();expect([meta.width,meta.height]).toEqual(row.view==='fabric'?[1254,1254]:[1122,1402]);
 expect(readFileSync(`${process.env.APPAREL_SOURCE_DIR}/${row.acceptance}`,'utf8').split(/\r?\n/)[row.reviewLine-1]).toBe(row.reviewRow);
 }
});
test('C21 contains exactly 81 independent accepted slots and eight AI captions',()=>{expect(accepted.length).toBe(81);expect(new Set(accepted.map(r=>r.src)).size).toBe(81);expect(Object.keys(detailCaption).length).toBe(8);});
test.skipIf(!process.env.APPAREL_SOURCE_DIR)('C21 fabric packaging retains every native pixel dimension',async()=>{
 const {packageDetails}=await import('../detail-images');
 const ledger=(await import('../c21-ledger.json')).default;
 const receipts=await packageDetails(details.filter(d=>accepted.some(r=>r.src===d.src)) as any,process.env.APPAREL_SOURCE_DIR!,'web/apparel/dist',ledger);
 expect(receipts.length).toBe(34);for(const r of receipts) expect([r.w,r.h]).toEqual([1254,1254]);
});
