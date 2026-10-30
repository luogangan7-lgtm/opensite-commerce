/** Output allow-list and text gates; CI only; any unlisted product route or image reference fails closed. */
import { readdirSync, readFileSync } from 'node:fs';
import { join, relative } from 'node:path';
import products from '../src/data/products.json';
const root = process.env.APPAREL_DIST || 'web/apparel/dist';
function files(path: string): string[] { return readdirSync(path, { withFileTypes: true }).flatMap(e => e.isDirectory() ? files(join(path, e.name)) : [join(path, e.name)]); }
const allowed = new Set(products.map(p => p.slug));
const images = new Set(products.flatMap(p => [...p.images.map(i => i.src), ...p.size_chart]));
if (products.length !== 68 || allowed.size !== 68) throw Error('Published catalog count changed');
const source = JSON.stringify(products);
if (/"(?:original|web|rights|name_zh|collection_zh)"/.test(source) || source.includes('/' + 'Volumes/')) throw Error('Private catalog field');
for (const p of products) for (const key of ['price', 'moq', 'lead_time'] as const) if (p[key] !== 'Confirmed in quote') throw Error('Commercial value');
for (const file of files(root)) {
  const path = relative(root, file).replaceAll('\\', '/');
  if (path.startsWith('products/') && path.endsWith('/index.html') && !allowed.has(path.split('/')[1])) throw Error('Unpublished product route');
  if (!/\.(?:html|js|css|json)$/.test(path)) continue;
  const text = readFileSync(file, 'utf8');
  if (/mgoo/i.test(text) || /\baston\b/i.test(text) || text.includes('/' + 'Volumes/')) throw Error('Forbidden published text');
  for (const image of text.matchAll(/(?:src|href)="(\/img\/[^"?#]+)/g)) if (!images.has(image[1])) throw Error('Unpublished image reference');
  if (path.endsWith('.html')) {
    const plain = text.replace(/<script\b[^>]*>[\s\S]*?<\/script>/gi, '').replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ');
    if (/(?:price|moq|lead time|sample cost)\s*:?\s*(?:[$€£¥]|\d)|(?:[$€£¥]|\b(?:USD|EUR|GBP|CNY))\s*\d/i.test(plain)) throw Error('Numeric commercial term');
    for (const row of text.matchAll(/<tr>\s*<th[^>]*>(Price|MOQ|Lead time|Sample cost)<\/th>\s*<td[^>]*>(.*?)<\/td>\s*<\/tr>/gi)) if (row[2] !== 'Confirmed in quote') throw Error('Commercial table changed');
  }
}
for (const slug of allowed) if (!files(root).includes(join(root, 'products', slug, 'index.html'))) throw Error('Missing published page');
console.log('PASS: 68 allowed products; approved image references; no forbidden brand/local paths/numeric commercial terms.');
