/** C24 binds accepted R11 native replacements to their existing W3 gallery slots. */
import {test,expect} from 'bun:test';
import accepted from './accepted-c24.json';
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

test('C24 exactly 23 W3 replacements and eight AI captions',()=>{expect(accepted.length).toBe(23);expect(new Set(accepted.map(r=>r.src)).size).toBe(23);expect(Object.keys(detailCaption).length).toBe(8);});

// Built output confirms each replacement remains reachable in every localized PDP.
test('C24 derived counts and eight-locale rendered slots',()=>{
 expect(digital.length).toBe(767);expect(digital.filter(d=>d.wave==='W3').length).toBe(230);expect(details.length).toBe(230);
 for(const row of accepted) for(const locale of ['','zh/','es/','fr/','de/','pt/','ru/','ar/']) {
 const html=readFileSync(`web/apparel/dist/${locale}products/${row.slug}/index.html`,'utf8');
 expect(html).toContain(row.src);expect(html).toContain('data-role="'+row.view+'"');
 }
});

test.skipIf(!process.env.APPAREL_SOURCE_DIR)('C24 real digital packager binds masters and downscales derivatives',async()=>{
 const {packageDigital}=await import('../digital-images');
 const {mkdtempSync,rmSync}=await import('node:fs');
 const {resolve}=await import('node:path');
 const destination=mkdtempSync(resolve('web/apparel/.c24-packaging-'));
 try {
 for(const row of accepted){
 const descriptor=digital.find(d=>d.src===row.src)!;
 const bytes=readFileSync(`${process.env.APPAREL_SOURCE_DIR}/${row.source}`);
 expect(createHash('sha256').update(bytes).digest('hex')).toBe(row.sha256);
 const master=await sharp(bytes).metadata();expect([master.width,master.height]).toEqual([1122,1402]);
 const output=await packageDigital(descriptor,process.env.APPAREL_SOURCE_DIR!,destination);
 const meta=await sharp(output).metadata();
 expect(meta.format).toBe('webp');expect(meta.width).toBe(1024);
 expect(meta.height).toBe(Math.round(1402*1024/1122));
 expect(meta.width!).toBeLessThanOrEqual(master.width!);expect(meta.height!).toBeLessThanOrEqual(master.height!);
 expect(createHash('sha256').update(readFileSync(`${process.env.APPAREL_SOURCE_DIR}/${row.source}`)).digest('hex')).toBe(row.sha256);
 }
 // A smaller input exercises the same production path's never-upscale option.
 const small=await sharp({create:{width:512,height:640,channels:3,background:'white'}}).png().toBuffer();
 const {writeFileSync}=await import('node:fs');writeFileSync(`${destination}/small.png`,small);
 const output=await packageDigital({source:'small.png',src:'/small.webp',sha256:createHash('sha256').update(small).digest('hex')},destination,destination);
 const meta=await sharp(output).metadata();expect([meta.width,meta.height]).toEqual([512,640]);
 } finally {rmSync(destination,{recursive:true,force:true});}
});
