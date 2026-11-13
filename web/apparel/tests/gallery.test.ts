/** §6.2/RV2: primary-colour slots, exact ledger identity and native detail pixels. Synthetic fixtures only. */
import { test, expect } from 'bun:test';
import { mkdtempSync, rmSync, existsSync, readFileSync, writeFileSync } from 'node:fs';
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
const digest = (path: string) => createHash('sha256').update(readFileSync(path)).digest('hex');
const ledgerFor = (rows: DetailSource[]) => rows.map(r => ({style_slug:r.slug, source_slug:r.slug, colour:r.colour, logical_slot:`pdp.${r.role}`, asset_path:`/fixture/work/apparel-b2b/${r.source}`, asset_sha256:r.sha256, source_sha256:r.sourceSha256, accepted_evidence:`/fixture/work/apparel-b2b/${r.acceptance}`}));
test('primary slots are ordered, other colours require selection, unknown roles are excluded', () => {
 const rows=['fabric','print','garment','back','structure','turn','front'].map(role=>({src:role,role,colour:'cream'}));
 const all=[{src:'black-front',role:'front',colour:'black'},...rows,{src:'unknown',role:'overview',colour:'cream'},{src:'no-colour',role:'fabric'}];
 const before=JSON.stringify(all);
 expect(orderedGallery(all,'cream','cream').map(i=>i.src)).toEqual(['front','turn','back','garment','print','structure','fabric']);
 expect(orderedGallery(all,'black','cream').map(i=>i.src)).toEqual(['black-front']);
 expect(JSON.stringify(all)).toBe(before);
 for(const role of ['print','fabric','structure']) expect(isDetail({src:'x',role})).toBe(true);
 for(const caption of Object.values(detailCaption)) expect(caption).not.toMatch(/stitch|\bgsm\b|composition|针数|成分|克重/i);
});
test('G plan requires exact asset and original source SHA from the acceptance ledger', () => {
 const rows=details as DetailSource[], ledger=ledgerFor(rows);
 const input={slots:{},review:[],provenance:{},published:products.map(p=>p.slug),details:rows,detailLedger:ledger};
 expect(imagePlan(input).details).toHaveLength(80);
 expect(()=>imagePlan({...input,detailLedger:[]})).toThrow(/ledger/i);
 for(const patch of [{sha256:'b'.repeat(64)},{sourceSha256:'b'.repeat(64)},{acceptance:'forged.md'},{colour:'wrong'}]) expect(()=>imagePlan({...input,details:[{...rows[0],...patch}]})).toThrow(/ledger/i);
 expect(()=>imagePlan({...input,detailLedger:[ledger[0],ledger[0]],details:[rows[0]]})).toThrow(/ledger/i);
 for(const row of rows) {
  const p=products.find(p=>p.slug===row.slug)!;
  // #552: catalog binding makes the only front red; accepted cream fabric must not leak into it.
  const creamBackOnly = row.slug === 'crew-fleece-8900-53' && row.src === '/img/crew-fleece-8900-53/fabric.webp';
  if (creamBackOnly) {
   expect(row.colour).toBe('cream');
   expect(p.images.find(i=>i.src==='/img/crew-fleece-8900-53/digital-front.webp')?.role).toBe('back');
   expect(p.images.find(i=>i.role==='front')?.colour).toBe('red');
  }
  expect(orderedGallery(p.images).some(i=>i.src===row.src)).toBe(!creamBackOnly);
 }
});
test('64px detail is RED; native 1200 is GREEN; tampered file and source SHA fail before writing', async () => {
 const root=mkdtempSync(join(tmpdir(),'c4-detail-'));
 try {
  for(const size of [64,1200]) await sharp({create:{width:size,height:size,channels:3,background:'#cfcfcf'}}).png().toFile(join(root,`${size}.png`));
  const row=(size:number)=>({...details[0],source:`${size}.png`,sha256:digest(join(root,`${size}.png`))}) as DetailSource;
  await expect(packageDetails([row(64)],root,join(root,'small'),ledgerFor([row(64)]))).rejects.toThrow(/1200/);
  expect(existsSync(join(root,'small'))).toBe(false);
  const good=row(1200), ledger=ledgerFor([good]);
  const receipts=await packageDetails([good],root,join(root,'good'),ledger);
  expect([receipts[0].w,receipts[0].h]).toEqual([1200,1200]);
  expect((await sharp(join(root,'good',good.src)).metadata()).format).toBe('webp');
  await expect(packageDetails([{...good,sourceSha256:'b'.repeat(64)}],root,join(root,'source-forged'),ledger)).rejects.toThrow(/ledger/i);
  writeFileSync(join(root,'1200.png'),readFileSync(join(root,'64.png')));
  await expect(packageDetails([good],root,join(root,'tampered'),ledger)).rejects.toThrow(/changed|ledger/i);
  // Replacing both the file AND its descriptor SHA must still fail against the unchanged ledger.
  await expect(packageDetails([{...good,sha256:digest(join(root,'1200.png'))}],root,join(root,'forged'),ledger)).rejects.toThrow(/ledger/i);
  for(const dir of ['source-forged','tampered','forged']) expect(existsSync(join(root,dir))).toBe(false);
 } finally {rmSync(root,{recursive:true,force:true});}
});

test('tampered 1200 file with a re-signed descriptor cannot override the accepted ledger', async () => {
 const root=mkdtempSync(join(tmpdir(),'c4-ledger-'));
 try {
  const file=join(root,'asset.png');
  await sharp({create:{width:1200,height:1200,channels:3,background:'#fff'}}).png().toFile(file);
  const accepted={...details[0],source:'asset.png',sha256:digest(file)} as DetailSource;
  const ledger=ledgerFor([accepted]);
  await sharp({create:{width:1200,height:1200,channels:3,background:'#000'}}).png().toFile(file);
  const forged={...accepted,sha256:digest(file)};
  await expect(packageDetails([forged],root,join(root,'out'),ledger)).rejects.toThrow(/ledger/i);
  expect(existsSync(join(root,'out'))).toBe(false);
 } finally {rmSync(root,{recursive:true,force:true});}
});

test('print fallback rejects non-1200 native input before copying, then accepts exact1200', async () => {
 const {copyFallbackImage}=await import('../detail-images');
 const root=mkdtempSync(join(tmpdir(),'c4-print-'));
 try {
  for(const size of [64,1200]) await sharp({create:{width:size,height:size,channels:3,background:'#fff'}}).webp().toFile(join(root,`${size}.webp`));
  const source=join(root,'64.webp'), output=join(root,'bad','print.webp');
  await expect(copyFallbackImage({role:'print',web_sha256:digest(source)},source,output)).rejects.toThrow(/1200/);
  expect(existsSync(output)).toBe(false);
  const good=join(root,'1200.webp'), target=join(root,'good','print.webp');
  const receipt=await copyFallbackImage({role:'print',web_sha256:digest(good)},good,target);
  expect([receipt.w,receipt.h]).toEqual([1200,1200]);
  expect(readFileSync(target)).toEqual(readFileSync(good));
  // The stronger C gate must not reject an ordinary legacy fallback merely for its dimensions.
  expect((await copyFallbackImage({role:'model'},source,join(root,'legacy.webp'))).w).toBe(64);
 } finally {rmSync(root,{recursive:true,force:true});}
});
