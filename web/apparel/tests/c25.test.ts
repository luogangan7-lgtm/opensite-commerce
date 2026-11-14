/** C25 accepted R12 masters, gallery slots and production delivery packaging. */
import {test,expect} from 'bun:test';
import accepted from './accepted-c25.json';
import digital from '../digital-sources.json';
import details from '../detail-sources.json';
import products from '../src/data/products.json';
import {packageDigital} from '../digital-images';
import {packageDetails,readDetailLedger} from '../detail-images';
import {readFileSync,mkdtempSync,rmSync} from 'node:fs';
import {createHash} from 'node:crypto';
import {createRequire} from 'node:module';
const sharp=createRequire(import.meta.resolve('astro/package.json'))('sharp');
for(const row of accepted) test(row.exactId,async()=>{
 const descriptor=[...digital,...details].find(x=>x.src===row.src)!;
 expect(descriptor?.source).toBe(row.source);expect(descriptor?.sha256).toBe(row.sha256);
 const image=products.find(p=>p.slug===row.slug)!.images.find(x=>x.src===row.src)!;
 expect(image?.role).toBe(row.view);expect(image?.sha256_16).toBe(row.sha256.slice(0,16));
 if(row.replaced)expect(digital.some(x=>x.source===row.oldSource)).toBe(false);
 const root=process.env.APPAREL_SOURCE_DIR;if(!root)return;
 const bytes=readFileSync(`${root}/${row.source}`);expect(createHash('sha256').update(bytes).digest('hex')).toBe(row.sha256);
 expect(readFileSync(`${root}/${row.acceptance}`,'utf8').split(/\r?\n/)[row.reviewLine-1]).toBe(row.reviewRow);
 const master=await sharp(bytes).metadata();
 if(row.view==='fabric'){expect(master.width).toBe(master.height);expect(master.width!).toBeGreaterThanOrEqual(1200);}else expect([master.width,master.height]).toEqual([1122,1402]);
 const dest=mkdtempSync('web/apparel/.c25-');try{
 if(row.view==='fabric')await packageDetails([descriptor as any],root,dest,readDetailLedger(root));else await packageDigital(descriptor,root,dest);
 const meta=await sharp(`${dest}${row.src}`).metadata();expect(meta.format).toBe('webp');expect(meta.width).toBe(row.view==='fabric'?master.width:1024);
 expect(createHash('sha256').update(readFileSync(`${root}/${row.source}`)).digest('hex')).toBe(row.sha256);
 }finally{rmSync(dest,{recursive:true,force:true});}
});
test('C25 nine accepted slots, eight composite replacements and one new fabric',()=>{
 expect(accepted.length).toBe(9);expect(accepted.filter(r=>r.replaced).length).toBe(8);
 expect(digital.length).toBe(767);expect(details.length).toBe(230);
});
