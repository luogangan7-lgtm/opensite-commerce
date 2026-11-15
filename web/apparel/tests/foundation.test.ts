/** Route and query contract; catalog is the only product allow-list. */
import { test, expect } from 'bun:test';
import { locales, pagePath } from '../src/i18n';
import { products } from '../src/data/catalog';
import { parseRfqPrefill } from '../src/data/rfq-prefill';
test('eight languages and zh path stripping', () => {
  expect([...locales]).toEqual(['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar']);
  expect(pagePath(new URL('https://example.invalid/zh/contact/?type=sample'))).toBe('/contact/');
});
test('prefill accepts only catalog products and contract enums', () => {
  const p = products[0];
  expect(parseRfqPrefill(new URLSearchParams({product:p.slug,type:'sample',subject:'fabric-photos'}))).toEqual({product:p, type:'sample',subject:'fabric-photos'});
  for (const subject of ['full-range','lining-photos','reference-photo']) expect(parseRfqPrefill(new URLSearchParams({subject})).subject).toBe(subject);
  expect(parseRfqPrefill(new URLSearchParams('product=<script>&type=evil&subject=evil'))).toEqual({});
  expect(parseRfqPrefill(new URLSearchParams('product=x&product=y&type=sample&type=evil&subject=full-range&subject=evil'))).toEqual({});
  expect(parseRfqPrefill(new URLSearchParams())).toEqual({});
});
test('prefill accepts only one catalog colour belonging to the selected product', () => {
  const p = products.find(p => p.images.some(i => i.colour))!;
  const colour = p.images.find(i => i.colour)!.colour;
  expect(parseRfqPrefill(new URLSearchParams({product:p.slug,colour})).colour).toBe(colour);
  expect(parseRfqPrefill(new URLSearchParams({product:p.slug,colour:'<script>'})).colour).toBeUndefined();
  expect(parseRfqPrefill(new URLSearchParams(`product=${p.slug}&colour=${colour}&colour=${colour}`)).colour).toBeUndefined();
});
