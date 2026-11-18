/** Frozen FD1 whole/G contract: all 115 PDPs use their own accepted primary media. */
import { test, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { pdpMediaPair } from '../src/data/gallery';
import products from '../src/data/products.json';
import digital from '../digital-sources.json';
import details from '../detail-sources.json';

test('every PDP pairs its own accepted W1 front with same-colour G structure', () => {
 expect(products).toHaveLength(115);
 for (const product of products) {
  const { whole, detail } = pdpMediaPair(product.images);
  expect(whole.role).toBe('front'); expect(detail.role).toBe('structure');
  expect(whole.colour).toBe(detail.colour);
  expect(digital.some(row => row.slug === product.slug && row.wave === 'W1' && row.src === whole.src)).toBe(true);
  expect(details.some(row => row.slug === product.slug && row.colour === detail.colour && row.src === detail.src)).toBe(true);
 }
});
test('missing same-colour media fails rather than substituting another SKU or colour', () => {
 expect(() => pdpMediaPair([{src:'front',role:'front',colour:'white'},{src:'detail',role:'structure',colour:'black'}])).toThrow();
});
test('PDP replaces generic craft illustrations with FIT pair', () => {
 const view=readFileSync(new URL('../src/components/ProductGallery.astro',import.meta.url),'utf8');
 expect(view).not.toContain('pdp/craft_'); expect(view).toContain('data-pdp-media');
 expect(view).toMatch(/object-fit:\s*contain/);
});
