/** Gallery contracts and real image transformations; fixtures stay isolated and are removed. */
import { test, expect } from 'bun:test';
import { mkdtempSync, rmSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createRequire } from 'node:module';
import { createHash } from 'node:crypto';
import { orderedGallery, isDetail, detailCaption } from '../src/data/gallery';
import { imagePlan, type DetailSource } from '../image-plan';
import { packageDetails } from '../detail-images';
import details from '../detail-sources.json';
import products from '../src/data/products.json';
const sharp = createRequire(import.meta.resolve('astro/package.json'))('sharp');
test('explicit gallery sequence, legacy defaults, C/G fit and non-mutating order', () => {
 const roles = ['fabric','garment','print','back','structure','turn','front'];
 const rows = roles.map(role => ({src:role,role})); const original = [...rows];
 expect(orderedGallery([...rows,{src:'legacy'}]).map(i=>i.src)).toEqual(['front','turn','back','print','garment','structure','fabric','legacy']);
 expect(rows).toEqual(original);
 for (const role of ['print','fabric','structure']) expect(isDetail({src:'x',role})).toBe(true);
 for (const caption of Object.values(detailCaption)) expect(caption).not.toMatch(/stitch|\bgsm\b|composition|针数|成分|克重/i);
});
test('80 G bindings accept published identities and reject invalid sources/digests/duplicates', () => {
 const input = {slots:{}, review:[], provenance:{}, published:products.map(p=>p.slug), details:details as DetailSource[]};
 expect(imagePlan(input).details).toHaveLength(80);
 for (const patch of [{slug:'wrong'}, {sha256:'no'}, {source:'../outside.png'}, {role:'front'}, {src:'/img/other/fabric.webp'}]) expect(()=>imagePlan({...input,details:[{...details[0],...patch} as DetailSource]})).toThrow();
 expect(()=>imagePlan({...input,details:[details[0],details[0]] as DetailSource[]})).toThrow();
 for (const row of details) expect(products.find(p=>p.slug===row.slug)?.images.some(i=>i.src===row.src && i.role==='fabric' && i.colour===row.colour)).toBe(true);
});
test('packaging verifies all hashes before writing and never upscales or crops', async () => {
 const root=mkdtempSync(join(tmpdir(),'c4-detail-'));
 try {
  await sharp({create:{width:64,height:64,channels:3,background:'#cfcfcf'}}).png().toFile(join(root,'small.png'));
  const sha=createHash('sha256').update(new Uint8Array(await Bun.file(join(root,'small.png')).arrayBuffer())).digest('hex');
  const row={...details[0],source:'small.png',sha256:sha} as DetailSource;
  await expect(packageDetails([row,{...row,sha256:'b'.repeat(64)}],root,join(root,'bad'))).rejects.toThrow(/changed/);
  expect(existsSync(join(root,'bad'))).toBe(false);
  const receipts=await packageDetails([row],root,join(root,'out'));
  expect(receipts[0].w).toBe(64);expect(receipts[0].h).toBe(64);
  expect((await sharp(join(root,'out',row.src)).metadata()).format).toBe('webp');
 } finally {rmSync(root,{recursive:true,force:true});}
});
