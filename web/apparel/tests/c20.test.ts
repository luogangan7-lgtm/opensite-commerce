/** C20 accepted W3 contract: exact sources, primary gallery and native packaging. */
import {test,expect} from 'bun:test';
import rows from './accepted-c20.json';
import digital from '../digital-sources.json';
import products from '../src/data/products.json';
import {orderedGallery} from '../src/data/gallery';
import {readFileSync} from 'node:fs';
import {createHash} from 'node:crypto';
import {join} from 'node:path';
import {packageDigital} from '../digital-images';
import {mkdtempSync,rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
test('19 accepted W3 slots are SHA-bound and visible in primary ordered galleries',()=>{
 for(const row of rows){
 const role=row.slot.split('_')[1],src=`/img/${row.slug}/digital-${role}.webp`;
 expect(digital.find(d=>d.src===src)?.sha256,row.slug).toBe(row.sha256);
 expect(orderedGallery(products.find(p=>p.slug===row.slug)!.images).find(i=>i.src===src)?.role).toBe(role);
 }
});
test.skipIf(!process.env.APPAREL_SOURCE_DIR)('accepted originals package byte-for-byte at native dimensions',async()=>{
 const out=mkdtempSync(join(tmpdir(),'c20-'));
 try{for(const row of rows){
 const role=row.slot.split('_')[1],source=digital.find(d=>d.src===`/img/${row.slug}/digital-${role}.webp`)!;
 const bytes=readFileSync(join(process.env.APPAREL_SOURCE_DIR!,source.source));
 expect(createHash('sha256').update(bytes).digest('hex')).toBe(row.sha256);
 await packageDigital(source,bytes,join(out,'image.webp'));
 expect(readFileSync(join(out,'image.webp'))).toEqual(bytes);
 }}finally{rmSync(out,{recursive:true,force:true});}
});
import {catalogColours} from '../catalog-colours';
test('C19 DATA preserves Chinese compound colours and excludes rear-view descriptors',()=>{
 expect(catalogColours('灰/黑/藏青')).toEqual(['灰','黑','藏青']);
 expect(catalogColours(['light grey-green/sage','black','navy','khaki/sand','light grey (back view, img 5)'])).toEqual(['light grey-green/sage','black','navy','khaki/sand']);
});
