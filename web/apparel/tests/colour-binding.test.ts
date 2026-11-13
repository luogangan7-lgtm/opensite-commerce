/** C10 data contract: expectations transcribed from AGGREGATE all + catalog colourways by index;
 * AGGREGATE SHA256 f6ee705d6d5068cdf0bfc77cacbedf63a9414f3f5473841738325524f17c8a63.
 * Catalog SHA256 fef2fe824d702665a80110c714459023798fafd5d54669235d0caae1a2aa4876.
 * source paths resolved through digital-sources, never guessed from source_model filenames.
 * Consumed by browser tests too; R5 accepted-r5.json fills 22 previous null mappings; remaining null mappings stay unassigned and back roles are preserved.
 */
import { test, expect } from 'bun:test';
import products from '../src/data/products.json';
import digital from '../digital-sources.json';
import bindings from './colour-bindings.json';
import { galleryColours, orderedGallery } from '../src/data/gallery';
const backOnly = new Set(['/img/crew-fleece-8900-53/digital-front.webp', '/img/hoodie-set-k21-70046/digital-model-05.webp']);
test('all 496 bound catalog colours resolve to existing product and accepted image entries', () => {
  const bound = bindings.filter(row => row.src);
  expect(bound).toHaveLength(496);
  let selectable = 0;
  for (const row of bound) {
    const product = products.find(p => p.slug === row.slug)!;
    expect(product).toBeDefined();
    const image = product.images.find(image => image.src === row.src)!;
    expect(image).toBeDefined();
    expect(digital.some(image => image.slug === row.slug && image.src === row.src)).toBe(true);
    expect(image.colour).toBe(row.colour);
    if (backOnly.has(row.src!)) {
      expect(image.role).toBe('back');
      expect(galleryColours(product.images).some(image => image.src === row.src)).toBe(false);
    } else {
      expect(galleryColours(product.images).find(image => image.colour === row.colour)?.src).toBe(row.src!);
      expect(orderedGallery(product.images, row.colour).find(image => image.role === 'front')?.src).toBe(row.src!);
      selectable++;
    }
  }
  expect(selectable).toBe(494);
});
test('remaining null mapping do not invent a front binding', () => {
  const missing = bindings.filter(row => !row.src);
  expect(missing).toHaveLength(1);
  for (const row of missing) {
    const product = products.find(p => p.slug === row.slug)!;
    expect(galleryColours(product.images).some(image => image.colour === row.colour)).toBe(false);
  }
});
