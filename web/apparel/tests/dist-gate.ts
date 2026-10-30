/** Output allow-list and text gates; CI only; any unlisted product route or image reference fails closed. */
import { readdirSync, readFileSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import products from '../src/data/products.json';
import { dictionaries, locales, type Locale } from '../src/i18n';
const root = process.env.APPAREL_DIST || 'web/apparel/dist';
// Pages decides whether to invoke the compiled Worker from this manifest, before dispatch.
const routes = JSON.parse(readFileSync(join(root, '_routes.json'), 'utf8'));
if (routes.version !== 1 || routes.exclude.length !== 0 || JSON.stringify([...routes.include].sort()) !== JSON.stringify(['/api/rfq', '/contact', '/contact/'])) throw Error('Pages route manifest must invoke RFQ and both contact paths');
function files(path: string): string[] { return readdirSync(path, { withFileTypes: true }).flatMap(e => e.isDirectory() ? files(join(path, e.name)) : [join(path, e.name)]); }
const allowed = new Set(products.map(p => p.slug));
const images = new Set(products.flatMap(p => [...p.images.map(i => i.src), ...p.size_chart]));
if (products.length !== 68 || allowed.size !== 68) throw Error('Published catalog count changed');
const source = JSON.stringify(products);
if (/"(?:original|web|rights|name_zh|collection_zh)"/.test(source) || source.includes('/' + 'Volumes/')) throw Error('Private catalog field');
for (const p of products) for (const key of ['price', 'moq', 'lead_time'] as const) if (p[key] !== 'Confirmed in quote') throw Error('Commercial value');
for (const file of files(root)) {
  const path = relative(root, file).replaceAll('\\', '/');
  const parts = path.split('/');
  const locale: Locale = locales.includes(parts[0] as Locale) ? parts.shift() as Locale : 'en';
  if (parts[0] === 'products' && path.endsWith('/index.html') && !allowed.has(parts[1])) throw Error('Unpublished product route');
  if (!/\.(?:html|js|css|json)$/.test(path)) continue;
  const text = readFileSync(file, 'utf8');
  if (/mgoo/i.test(text) || /\baston\b/i.test(text) || text.includes('/' + 'Volumes/')) throw Error('Forbidden published text');
  for (const image of text.matchAll(/(?:src|href)="(\/img\/[^"?#]+)/g)) if (!images.has(image[1])) throw Error('Unpublished image reference');
  if (/tryon-|数字人形象|assets\/tryon/.test(text)) throw Error('Forbidden image provenance');
  if (path.endsWith('.html')) {
    if (!text.includes(`lang="${locale}"`) || !text.includes(`dir="${locale === 'ar' ? 'rtl' : 'ltr'}"`)) throw Error('Wrong document locale');
    const alternates = [...text.matchAll(/<link rel="alternate" hreflang="([^"]+)" href="([^"]+)"/g)];
    if (alternates.length !== 8 || new Set(alternates.map(row => row[1])).size !== 8 || ![...locales, 'x-default'].every(lang => alternates.some(row => row[1] === lang))) throw Error('Incomplete hreflang');
    for (const row of text.matchAll(/<tr data-commercial[^>]*>[\s\S]*?<td[^>]*>(.*?)<\/td>/g)) if (row[1] !== dictionaries[locale].quote) throw Error('Localized commercial value');
    const plain = text.replace(/<script\b[^>]*>[\s\S]*?<\/script>/gi, '').replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ');
    if (/(?:price|moq|lead time|sample cost)\s*:?\s*(?:[$€£¥]|\d)|(?:[$€£¥]|\b(?:USD|EUR|GBP|CNY))\s*\d/i.test(plain)) throw Error('Numeric commercial term');
    for (const row of text.matchAll(/<tr>\s*<th[^>]*>(Price|MOQ|Lead time|Sample cost)<\/th>\s*<td[^>]*>(.*?)<\/td>\s*<\/tr>/gi)) if (row[2] !== dictionaries[locale].quote) throw Error('Commercial table changed');
  }
}
for (const locale of locales) for (const slug of allowed) if (!files(root).includes(join(root, locale === 'en' ? '' : locale, 'products', slug, 'index.html'))) throw Error('Missing published locale page');
console.log('PASS: 68 allowed products in seven locales; approved image references; localized commercial terms.');

// Exercise the exact compiled Pages dispatch, not just the TypeScript source handler.
const worker = (await import(pathToFileURL(resolve(root, '_worker.js/index.js')).href)).default;
const records: object[] = [];
const env = { SUPABASE_URL: 'https://database.example.invalid', SUPABASE_SECRET_KEY: 'ci-private-placeholder', ASSETS: { fetch: (request: Request) => {
  const path = new URL(request.url).pathname;
  if (path === '/contact/') return new Response('Contact RFQ', { status: 200 });
  if (path === '/contact' || path === '/about') return new Response(null, { status: 308, headers: { Location: `${path}/` } });
  return new Response('not found', { status: 404 });
} } };
globalThis.fetch = async (input, init) => {
  if (init?.redirect === 'error') throw new TypeError('Unsupported redirect mode');
  const request = new Request(input, init);
  if (request.url !== `${env.SUPABASE_URL}/rest/v1/apparel_rfq` || request.method !== 'POST' || request.redirect !== 'manual' || request.headers.get('apikey') !== env.SUPABASE_SECRET_KEY || request.headers.get('Authorization') !== `Bearer ${env.SUPABASE_SECRET_KEY}` || request.headers.get('Prefer') !== 'return=representation') throw Error('Compiled PostgREST contract failed');
  records.push(await request.json());
  return Response.json([{ id: '524f5000-0000-4000-8000-000000000001' }], { status: 201 });
};
const context = { waitUntil: () => {}, passThroughOnException: () => {} };
for (const [email, website, expected] of [['gate@example.invalid', '', 201], ['invalid', '', 400], ['gate@example.invalid', 'bot', 201]] as const) {
  const response = await worker.fetch(new Request('https://example.invalid/api/rfq', { method: 'POST', body: JSON.stringify({ name: 'CI Gate', company: 'Example', email, website, message: 'Synthetic compiled-worker inquiry.' }) }), env, context);
  if (response.status !== expected) throw Error('Compiled RFQ route failed');
}
if (records.length !== 1) throw Error('Compiled RFQ storage contract failed');
for (const [path, expected] of [['/contact', 200], ['/contact/', 200], ['/about', 308]] as const) {
  const response = await worker.fetch(new Request(`https://example.invalid${path}`), env, context);
  if (response.status !== expected) throw Error(`Compiled route ${path} expected ${expected}, got ${response.status}`);
}
console.log('PASS: compiled Pages dispatch, valid/invalid/honeypot RFQ, private PostgREST insert and contact 200.');
