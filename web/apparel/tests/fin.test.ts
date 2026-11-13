/** FIN pool provenance and the accepted W2 beige front use existing G/digital boundaries. */
import {test,expect} from 'bun:test';
import accepted from './accepted-fin.json';
import details from '../detail-sources.json';
import digital from '../digital-sources.json';
import products from '../src/data/products.json';
import bindings from './colour-bindings.json';
import metadata from '../src/data/digital-imagery.json';
import {orderedGallery} from '../src/data/gallery';
import {assertDigitalDigest} from '../image-plan';
import {readFileSync,mkdirSync} from 'node:fs';
import {join,dirname} from 'node:path';
import {createHash} from 'node:crypto';
import {createRequire} from 'node:module';
const sharp=createRequire(import.meta.resolve('astro/package.json'))('sharp');
test('all six FIN PASS entries bind exact source SHAs and colour-selected gallery images',()=>{
 expect(accepted).toHaveLength(6);
 expect(accepted.filter(r=>r.role==='structure')).toHaveLength(5);
 for(const row of accepted){
  const source=[...details,...digital].find(i=>i.src===row.src)!;
  expect(source.sha256).toBe(row.sha256);expect(source.source).toBe(row.source);expect(source.acceptance).toBe(row.acceptance);
  const image=orderedGallery(products.find(p=>p.slug===row.slug)!.images,row.colour).find(i=>i.src===row.src)!;
  expect(image.role).toBe(row.role);expect(image.colour).toBe(row.colour);expect(image.sha256_16).toBe(row.sha256.slice(0,16));
 }
 const row=accepted.find(r=>r.role==='front')!;
 expect(bindings.find(b=>b.slug===row.slug&&b.catalog_pointer_index===2)).toMatchObject({colour:'beige/sand',src:row.src});
 expect(metadata.find(m=>m.src===row.src)).toMatchObject({color:'t',view:'front',wave:'W2'});
});
test.skipIf(!process.env.APPAREL_SOURCE_DIR)('six FIN originals match SHA/review/native geometry; W2 packages without enlargement',async()=>{
 const root=process.env.APPAREL_SOURCE_DIR!;
 for(const row of accepted){
  const bytes=readFileSync(join(root,row.source));
  expect(createHash('sha256').update(bytes).digest('hex')).toBe(row.sha256);
  expect(readFileSync(join(root,row.acceptance),'utf8')).toContain(`${row.exactId}\t${row.sha256}\tPASS`);
  const meta=await sharp(bytes).metadata();expect([meta.width,meta.height]).toEqual([row.width,row.height]);
  if(row.role==='front'){
   const descriptor=digital.find(i=>i.src===row.src)!;assertDigitalDigest(descriptor,createHash('sha256').update(bytes).digest('hex'));
   const output=join('web/apparel/dist',row.src);mkdirSync(dirname(output),{recursive:true});
   await sharp(bytes).resize({width:1024,withoutEnlargement:true}).webp({quality:82}).toFile(output);
   const result=await sharp(output).metadata();expect([result.width,result.height]).toEqual([1024,1280]);
  }
 }
});
