/** Locale contract; CI only, independent eight-language inventory and complete product records. */
import { test, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import products from '../src/data/products.json';
const locales = ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar'];
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
      if (locale === 'ar') {
        const codes = product.style_codes.join(' / ');
        if (codes) expect(translated[product.slug].name).toContain(`\u2068${codes}\u2069`);
        expect(translated[product.slug].name).not.toContain('\u2068\u2069');
      }
      for (const value of Object.values(translated[product.slug])) expect(typeof value === 'string' && value.trim().length > 0).toBe(true);
    }
  }
});

test('K24/K25 preserve stand collar, zip jacket and trousers in all seven translations', () => {
  const terms = { zh: ['立领', '拉链夹克', '裤'], es: ['cuello alto', 'cremallera', 'pantalón'], fr: ['col montant', 'zippée', 'pantalon'], de: ['Stehkragen', 'Reißverschluss', 'Jogginghose'], pt: ['gola alta', 'zíper', 'calça'], ru: ['стойкой', 'молнии', 'брюк'], ar: ['ياقة قائمة', 'بسحاب', 'بنطال'] };
  for (const [locale, required] of Object.entries(terms)) for (const slug of ['hoodie-set-k24-tz2803-5', 'hoodie-set-k25-tz2803-6']) {
    const name = read(`products.${locale}`)[slug].name;
    for (const term of required) expect(name).toContain(term);
  }
});
test('empty-code titles retain catalog identity without inventing style codes', () => {
  for (const locale of locales) {
    const d = read(`products.${locale}`);
    expect(d['apparel-70174-x-small-logo'].name).toContain('70174');
    expect(d['apparel-70178-x-loropiana-crest'].name).toContain('70178');
    expect(d['hoodie-set-k01-0256-x-croc'].name).toContain('01_0256');
  }
});

// Reviewed wording is fixed against the English meaning; the shared contract above checks all eight dictionaries.
import reviewedTranslations from './reviewed-translations.json';
test('cross-check fixes retain the reviewed wording and consent placeholders', () => {
  for (const {locale, key, new: expected} of reviewedTranslations) expect(read(locale)[key]).toBe(expected);
  for (const locale of locales) {
    expect(read(locale).cookieDetails).toContain('_ga_<ID>');
    expect(read(locale)['hoodie-set']).not.toBe(read(locale).sets);
  }
});

test('illustration alt descriptions are localized across all eight locales', () => {
  const en = read('en');
  for (const locale of locales.filter(l => l !== 'en')) for (const key of Object.keys(en).filter(k => k.startsWith('alt.'))) {
    expect(read(locale)[key], `${locale}:${key}`).not.toBe(en[key]);
  }
});
test('Arabic fleece descriptions never claim wool fibre', () => {
  expect(JSON.stringify([read('ar'), read('products.ar')])).not.toMatch(/صوف/);
});
test('all localized product card and page content is free of placeholders', () => {
  for (const locale of locales) for (const product of Object.values(read(`products.${locale}`))) {
    expect(JSON.stringify(product)).not.toMatch(/\b(?:TBD|TODO|placeholder|lorem ipsum)\b/i);
  }
});
