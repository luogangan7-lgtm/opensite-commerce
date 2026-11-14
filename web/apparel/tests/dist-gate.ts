/** Output allow-list and text gates; CI only; any unlisted product route or image reference fails closed. */
import { readdirSync, readFileSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import digital from '../digital-sources.json';
import { digitalDelivery } from '../image-plan';
import products from '../src/data/products.json';
import { dictionaries, locales, type Locale } from '../src/i18n';
import { imageBase, widths } from '../src/data/imagery';
const root = process.env.APPAREL_DIST || 'web/apparel/dist';
const policy = JSON.parse(readFileSync(join(root, 'rfq-policy.json'), 'utf8'));
if (typeof policy.required !== 'boolean' || (process.env.SEO_BUILD_MODE === 'production' && !policy.required)) throw Error('Turnstile production policy missing');
// Pages decides whether to invoke the compiled Worker from this manifest, before dispatch.
const routes = JSON.parse(readFileSync(join(root, '_routes.json'), 'utf8'));
if (routes.version !== 1 || routes.exclude.length !== 0 || JSON.stringify([...routes.include].sort()) !== JSON.stringify(['/api/rfq', '/contact', '/contact/'])) throw Error('Pages route manifest must invoke RFQ and both contact paths');
function files(path: string): string[] { return readdirSync(path, { withFileTypes: true }).flatMap(e => e.isDirectory() ? files(join(path, e.name)) : [join(path, e.name)]); }
const allowed = new Set(products.map(p => p.slug));
const images = new Set(products.flatMap(p => [...p.images.map(i => i.src), ...p.size_chart]));
// Admit exact derivatives only when the accepted master is bound to a published catalog image.
for (const master of digital) {
 const published = products.find(product => product.slug === master.slug)?.images.find(image => image.src === master.src);
 if (!published || published.sha256_16 !== master.sha256.slice(0,16)) throw Error('Digital master differs from published catalog');
 for (const delivery of digitalDelivery(master)) images.add(delivery.src);
}
// Dictionary-backed slot references only; source review stays deploy-only.
for (const key of Object.keys(dictionaries.en).filter(key => key.startsWith('alt.'))) for (const width of widths) for (const format of ['avif', 'webp']) images.add(`${imageBase(key.slice(4).replace('.', '/'))}-${width}.${format}`);
if (products.length !== 115 || allowed.size !== 115) throw Error('Published catalog count changed');
const source = JSON.stringify(products);
if (/"(?:original|web|rights|name_zh|collection_zh)"/.test(source) || source.includes('/' + 'Volumes/')) throw Error('Private catalog field');
for (const p of products) for (const key of ['price', 'moq', 'lead_time'] as const) if (p[key] !== 'Confirmed in quote') throw Error('Commercial value');
for (const file of files(root)) {
  const path = relative(root, file).replaceAll('\\', '/');
  const parts = path.split('/');
  const locale: Locale = locales.includes(parts[0] as Locale) ? parts.shift() as Locale : 'en';
  if (parts[0] === 'products' && path.endsWith('/index.html') && !allowed.has(parts[1])) throw Error('Unpublished product route');
  if (path.startsWith('_worker.js/')) continue; // Server bundle is not a client asset.
  if (!/\.(?:html|js|css|json)$/.test(path)) continue;
  const text = readFileSync(file, 'utf8');
  if (/mgoo/i.test(text) || /\baston\b/i.test(text) || text.includes('/' + 'Volumes/')) throw Error('Forbidden published text');
  for (const image of text.matchAll(/(?:src|href)="(\/img\/[^"?#]+)/g)) if (!images.has(image[1])) throw Error('Unpublished image reference');
  for (const srcset of text.matchAll(/(?:imagesrcset|srcset)="([^"]+)"/g)) for (const candidate of srcset[1].split(',')) if (!images.has(candidate.trim().split(/\s+/)[0])) throw Error('Unpublished responsive image');
  if (/tryon-|数字人形象|assets\/tryon/.test(text)) throw Error('Forbidden image provenance');
  if (path.endsWith('.html')) {
    const html = text.match(/<html\b[^>]*>/)?.[0] || '';
    if (!html.includes(`lang="${locale}"`) || !html.includes(`dir="${locale === 'ar' ? 'rtl' : 'ltr'}"`)) throw Error('Wrong document locale');
    const alternates = [...text.matchAll(/<link rel="alternate" hreflang="([^"]+)" href="([^"]+)"/g)];
    if (alternates.length !== 9 || new Set(alternates.map(row => row[1])).size !== 9 || ![...locales.map(l => l === 'zh' ? 'zh-Hans' : l), 'x-default'].every(lang => alternates.some(row => row[1] === lang))) throw Error('Incomplete hreflang');
    for (const row of text.matchAll(/<tr data-commercial[^>]*>[\s\S]*?<td[^>]*>(.*?)<\/td>/g)) if (row[1] !== dictionaries[locale].quote) throw Error('Localized commercial value');
    const plain = text.replace(/<script\b[^>]*>[\s\S]*?<\/script>/gi, '').replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ');
    if (/(?:price|moq|lead time|sample cost)\s*:?\s*(?:[$€£¥]|\d)|(?:[$€£¥]|\b(?:USD|EUR|GBP|CNY))\s*\d/i.test(plain)) throw Error('Numeric commercial term');
    for (const row of text.matchAll(/<tr>\s*<th[^>]*>(Price|MOQ|Lead time|Sample cost)<\/th>\s*<td[^>]*>(.*?)<\/td>\s*<\/tr>/gi)) if (row[2] !== dictionaries[locale].quote) throw Error('Commercial table changed');
  }
}
for (const locale of locales) for (const slug of allowed) if (!files(root).includes(join(root, locale === 'en' ? '' : locale, 'products', slug, 'index.html'))) throw Error('Missing published locale page');
console.log('PASS: 115 allowed products in eight locales; approved image references; localized commercial terms.');

// Exercise the exact compiled Pages dispatch, not just the TypeScript source handler.
const worker = (await import(pathToFileURL(resolve(root, '_worker.js/index.js')).href)).default;
const records: object[] = [];
const env = { SUPABASE_URL: 'https://database.example.invalid', SUPABASE_SECRET_KEY: 'ci-private-placeholder', ASSETS: { fetch: (request: Request) => {
  const path = new URL(request.url).pathname;
  if (path === '/rfq-policy.json') return Response.json({ required: false });
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
for (const [email, website, expected] of [['gate@example.invalid', '', 204], ['invalid', '', 400], ['gate@example.invalid', 'bot', 204]] as const) {
  const response = await worker.fetch(new Request('https://example.invalid/api/rfq', { method: 'POST', body: JSON.stringify({ name: 'CI Gate', company: 'Example', email, website, message: 'Synthetic compiled-worker inquiry.' }) }), env, context);
  if (response.status !== expected) throw Error('Compiled RFQ route failed');
}
if (records.length !== 1) throw Error('Compiled RFQ storage contract failed');
for (const [path, expected] of [['/contact', 200], ['/contact/', 200], ['/about', 308]] as const) {
  const response = await worker.fetch(new Request(`https://example.invalid${path}`), env, context);
  if (response.status !== expected) throw Error(`Compiled route ${path} expected ${expected}, got ${response.status}`);
}
console.log('PASS: compiled Pages dispatch, valid/invalid/honeypot RFQ, private PostgREST insert and contact 200.');
// Exercise the protected route through the packaged Pages worker as well.
const counters = new Map<string, Response>();
(globalThis as any).caches = { default: { match: async (r: Request) => counters.get(r.url)?.clone(), put: async (r: Request, v: Response) => { counters.set(r.url, v.clone()); } } };
let verified = false, protectedWrites = 0;
globalThis.fetch = async (input, init) => {
  if (new Request(input, init).url.includes('/siteverify')) return Response.json({ success: verified, hostname: 'example.invalid', action: 'rfq' });
  protectedWrites++;
  return Response.json([{ id: '524f5000-0000-4000-8000-000000000001' }], { status: 201 });
};
const protectedEnv = { ...env, TURNSTILE_SECRET_KEY: 'synthetic', ASSETS: { fetch: () => Response.json({ required: true }) } };
const submit = (bindings = protectedEnv) => worker.fetch(new Request('https://example.invalid/api/rfq', { method: 'POST', headers: { 'CF-Connecting-IP': '192.0.2.20' }, body: JSON.stringify({ name: 'CI', company: 'Example', email: 'ci@example.invalid', message: 'Synthetic', 'cf-turnstile-response': 'synthetic' }) }), bindings, context);
if ((await submit()).status !== 403 || protectedWrites !== 0) throw Error('Compiled Turnstile rejection');
verified = true;
for (let i = 0; i < 4; i++) if ((await submit()).status !== 204) throw Error('Compiled protected success');
if ((await submit()).status !== 429 || protectedWrites !== 4) throw Error('Compiled rate limit');
if ((await submit({ ...protectedEnv, TURNSTILE_SECRET_KEY: '' })).status !== 503) throw Error('Compiled production missing runtime secret');
console.log('PASS: compiled protected RFQ 403/204/429 and missing runtime secret 503; no storage on rejection.');
