/** CP-SVC source contract: owner-picked pins, localized technique alts, exact derivatives. */
import { test, expect } from 'bun:test';
import { createHash } from 'node:crypto';
import { digitalDelivery } from '../image-plan';
import { dictionaries, locales, type Key } from '../src/i18n';
import media from '../service-media.json';
test('sixteen owner-picked technique pins and localized names are preserved', () => {
 const techniques=media.filter(row=>row.kind==='technique');
 expect(techniques.map(row=>row.id)).toEqual(Array.from({length:16},(_,i)=>`tech-${String(i+1).padStart(2,'0')}`));
 for(const image of media) {
  expect(image.sha256).toMatch(/^[a-f0-9]{64}$/);
  expect(image.source.startsWith('/') || image.source.split('/').includes('..')).toBe(false);
  expect(digitalDelivery(image).map(file=>file.width)).toEqual([1024,640]);
 }
 for(const image of techniques) for(const locale of locales) expect(dictionaries[locale][image.altKey as Key]?.trim().length).toBeGreaterThan(0);
 // Bound to R-SVC2/TECH-FINAL.json, including the approved a2 replacements.
 const pins=techniques.map(({id,source,sha256})=>({id,source,sha256}));
 expect(createHash('sha256').update(JSON.stringify(pins)).digest('hex')).toBe('f0b4f081317a6c0e2df7a9864499b55abdd75857a5e4a4ab6fff67baa86852c7');
});
