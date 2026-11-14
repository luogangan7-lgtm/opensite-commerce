/** C23 binds accepted R11 native replacements to their existing W3 gallery slots. */
import {test,expect} from 'bun:test';
import accepted from './accepted-c23.json';
import digital from '../digital-sources.json';
import details from '../detail-sources.json';
import products from '../src/data/products.json';
import {orderedGallery,detailCaption} from '../src/data/gallery';
import {readFileSync} from 'node:fs';
import {createHash} from 'node:crypto';
import {createRequire} from 'node:module';
const sharp=createRequire(import.meta.resolve('astro/package.json'))('sharp');
for(const row of accepted) test(`${row.exactId}: F6-IMG slot and replacement source`,async()=>{
 const descriptor=[...digital,...details].find(d=>d.src===row.src);
 expect(descriptor?.sha256).toBe(row.sha256);expect(descriptor?.source).toBe(row.source);
 const image=orderedGallery(products.find(p=>p.slug===row.slug)!.images).find(i=>i.src===row.src);
 expect(image?.sha256_16).toBe(row.sha256.slice(0,16));expect(image?.role).toBe(row.view);
 if(row.replaced) expect(digital.some(d=>d.source===row.oldSource)).toBe(false);
 if(process.env.APPAREL_SOURCE_DIR){
 const bytes=readFileSync(`${process.env.APPAREL_SOURCE_DIR}/${row.source}`);
 expect(createHash('sha256').update(bytes).digest('hex')).toBe(row.sha256);
 const meta=await sharp(bytes).metadata();expect([meta.width,meta.height]).toEqual([1122,1402]);
 expect(readFileSync(`${process.env.APPAREL_SOURCE_DIR}/${row.acceptance}`,'utf8').split(/\r?\n/)[row.reviewLine-1]).toBe(row.reviewRow);
 }
});

test('C23 exactly 38 W3 replacements and eight AI captions',()=>{expect(accepted.length).toBe(38);expect(new Set(accepted.map(r=>r.src)).size).toBe(38);expect(Object.keys(detailCaption).length).toBe(8);});

// Built output confirms each replacement remains reachable in every localized PDP.
test('C23 derived counts and eight-locale rendered slots',()=>{
 expect(digital.length).toBe(767);expect(digital.filter(d=>d.wave==='W3').length).toBe(230);expect(details.length).toBe(229);
 for(const row of accepted) for(const locale of ['','zh/','es/','fr/','de/','pt/','ru/','ar/']) {
 const html=readFileSync(`web/apparel/dist/${locale}products/${row.slug}/index.html`,'utf8');
 expect(html).toContain(row.src);expect(html).toContain('data-role="'+row.view+'"');
 }
});
