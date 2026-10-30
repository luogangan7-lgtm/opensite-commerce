/** Locale contract; CI only, independent seven-language inventory and complete product records. */
import { test, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import products from '../src/data/products.json';
const locales = ['en', 'es', 'fr', 'de', 'pt', 'ru', 'ar'];
const read = (name: string) => JSON.parse(readFileSync(`web/apparel/src/i18n/${name}.json`, 'utf8'));
test('every UI dictionary has identical, nonempty keys and translated commercial text', () => {
  const english = read('en');
  for (const locale of locales) {
    const dictionary = read(locale);
    expect(Object.keys(dictionary).sort()).toEqual(Object.keys(english).sort());
    for (const value of Object.values(dictionary)) expect(typeof value === 'string' && value.trim().length > 0).toBe(true);
    expect(dictionary.quote).toBeTruthy();
    if (locale !== 'en') expect(dictionary.quote).not.toBe('Confirmed in quote');
  }
});
test('each published product has reviewed name and descriptions in every language', () => {
  for (const locale of locales) {
    const translated = read(`products.${locale}`);
    expect(Object.keys(translated).sort()).toEqual(products.map(p => p.slug).sort());
    for (const product of products) {
      expect(Object.keys(translated[product.slug]).sort()).toEqual(['colourway_note', 'fabric_weight_fit', 'name']);
      if (locale === 'ar') expect(translated[product.slug].name).toContain(`\u2068${product.style_codes.join(' / ')}\u2069`);
      for (const value of Object.values(translated[product.slug])) expect(typeof value === 'string' && value.trim().length > 0).toBe(true);
    }
  }
});
