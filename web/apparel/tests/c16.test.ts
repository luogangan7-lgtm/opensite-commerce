/** C16: primary-colour identity and reviewed structure selection. */
import { test, expect } from 'bun:test';
import products from '../src/data/products.json';
import imagery from '../src/data/digital-imagery.json';
import structures from '../detail-sources.json';
import { primaryColour, orderedGallery } from '../src/data/gallery';

test('cream 8900-53 and black 70190 select their accepted structures', () => {
  const cases = [
    ['crew-fleece-8900-53', 'cream', 'ca507fd5dae91ce9206852fe08cda161262e8eee8d6fe0f1ef5346138217c6e2'],
    ['apparel-70190', 'black', '2edb311285917a79fb35b7392cde35fe6e9f0269605e16cfbaf3ab29cfad05da'],
  ];
  for (const [slug, colour, sha] of cases) {
    const product = products.find(p => p.slug === slug)!;
    expect(primaryColour(product.images)).toBe(colour);
    const row = structures.find(r => r.slug === slug && r.role === 'structure');
    expect(row?.sha256).toBe(sha);
    expect(row?.colour).toBe(colour);
    expect(orderedGallery(product.images).find(i => i.role === 'structure')?.sha256_16).toBe(sha.slice(0, 16));
  }
  const front = imagery.find(i => i.src === '/img/crew-fleece-8900-53/digital-front.webp');
  const back = imagery.find(i => i.src === '/img/crew-fleece-8900-53/digital-back.webp');
  expect(front?.view).toBe('front');
  expect(back?.color).toBe('c');
});
