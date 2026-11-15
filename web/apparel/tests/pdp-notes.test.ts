/** PDP copy boundary: scans canonical and all localized product data; CI unit-suite discovers this file. */
import { test, expect } from 'bun:test';
import { readFileSync, readdirSync } from 'node:fs';

const internalNote = /folder name|filename|not stated|try-on render/i;
const files = ['web/apparel/src/data/products.json', ...readdirSync('web/apparel/src/i18n')
  .filter(name => /^products\.[^.]+\.json$/.test(name)).map(name => `web/apparel/src/i18n/${name}`)];

function strings(value: unknown): string[] {
  if (typeof value === 'string') return [value];
  if (Array.isArray(value)) return value.flatMap(strings);
  if (value && typeof value === 'object') return Object.values(value).flatMap(strings);
  return [];
}

for (const file of files) {
  test(`product copy contains no internal sourcing notes: ${file}`, () => {
    const leaked = strings(JSON.parse(readFileSync(file, 'utf8'))).filter(value => internalNote.test(value));
    expect(leaked).toEqual([]);
  });
}
