/** Delivery regression: mobile derivative preserves the approved master and aspect ratio. */
import { test, expect } from 'bun:test';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { createRequire } from 'node:module';
import { packageDigital } from '../digital-images';
const sharp = createRequire(import.meta.resolve('astro/package.json'))('sharp');
test('digital delivery includes a 640px candidate without altering the master', async () => {
 const dir = mkdtempSync('/Volumes/data/OpenSite Commerce/work/fx1-digital-');
 try {
  const input = `${dir}/master.png`;
  await sharp({create:{width:1200,height:1500,channels:3,background:'#abc'}}).png().toFile(input);
  const bytes = readFileSync(input), sha256 = createHash('sha256').update(bytes).digest('hex');
  await packageDigital({source:'master.png',src:'/img/test.webp',sha256},dir,dir);
  const mobile = await sharp(`${dir}/img/test-640.webp`).metadata();
  expect([mobile.width,mobile.height]).toEqual([640,800]);
  expect(readFileSync(input)).toEqual(bytes);
 } finally { rmSync(dir,{recursive:true,force:true}); }
});
