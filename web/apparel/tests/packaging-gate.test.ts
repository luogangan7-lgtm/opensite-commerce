/** Negative fixtures exercise the same public preflight CI calls; no private image sources needed. */
import { test, expect } from 'bun:test';
import { mkdirSync, mkdtempSync, writeFileSync, rmSync, copyFileSync } from 'node:fs';
import { join } from 'node:path';
import { packagingGate } from '../packaging-gate';
import { activeSlotInput } from '../image-plan';
import digital from '../digital-sources.json';
import { digitalAlt } from '../src/data/imagery';

test('unknown images, missing static assets, wrong localized alt and retired PDP slots fail then recover', () => {
 const dir=mkdtempSync(join(process.cwd(), 'wave6/FX-PKG/gate-fixture-'));
 const file=join(dir,'fr/products/fixture/index.html'), image=digital[0];
 const good=`<img src="${image.src}" alt="${digitalAlt(image.src,'fr')}"><img src="/brand/primary.svg" alt>`;
 const write=(html:string)=>writeFileSync(file,html);
 try {
  mkdirSync(join(dir,'fr/products/fixture'),{recursive:true}); mkdirSync(join(dir,'brand'));
  write(good); expect(()=>packagingGate(dir)).toThrow(/static asset/);
  copyFileSync('web/apparel/public/brand/primary.svg',join(dir,'brand/primary.svg'));
  expect(()=>packagingGate(dir)).not.toThrow();
  write(good.replace(image.src,'/img/unplanned.webp')); expect(()=>packagingGate(dir)).toThrow(/unplanned image/);
  write(good.replace(digitalAlt(image.src,'fr')!,digitalAlt(image.src,'en')!)); expect(()=>packagingGate(dir)).toThrow(/unlocalized alt/);
  write(good+ '<picture data-slot="pdp/craft_fleece"><img src="/img/site/illustrative_pdp_craft_fleece-1024.webp" alt="legacy"></picture>');
  expect(()=>packagingGate(dir)).toThrow(/Unpublished slot/);
  write(good); expect(()=>packagingGate(dir)).not.toThrow();
  expect(activeSlotInput({pdp:{craft:'legacy'},about:{hero:'current'},unknown:{hero:'invalid'}})).toEqual({about:{hero:'current'},unknown:{hero:'invalid'}});
 } finally { rmSync(dir,{recursive:true,force:true}); }
});
