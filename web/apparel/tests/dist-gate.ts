import { digitalLabel } from '../src/data/imagery';
/** Output allow-list and text gates; CI only; any unlisted product route or image reference fails closed. */
import { readdirSync, readFileSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import frozen from '../design/collection-frozen.json';
import digital from '../digital-sources.json';
import homeMedia from '../home-media.json';
import aboutMedia from '../about-media.json';
import contactMedia from '../contact-media.json';
import serviceMedia from '../service-media.json';
import { digitalDelivery } from '../image-plan';
import products from '../src/data/products.json';
import { dictionaries, locales, type Locale } from '../src/i18n';
import { pdpMoq, pdpMoqValue, aboutWays, aboutMoqCopy, shopCustomMoq, shopReadyMoq } from '../src/data/owner-facts';
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
// Frozen Home media: SHA-bound masters ship only their exact 1024/640 derivatives.
for (const master of [...homeMedia, ...aboutMedia, ...contactMedia, ...serviceMedia]) for (const delivery of digitalDelivery(master)) images.add(delivery.src);
for (const asset of frozen.assets) for (const file of asset.derivatives) images.add(file.src);

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
    if (text.includes('OWNER-INPUT')) throw Error(`Unresolved owner input in built HTML: ${path}`);
    const html = text.match(/<html\b[^>]*>/)?.[0] || '';
    if (!html.includes(`lang="${locale}"`) || !html.includes(`dir="${locale === 'ar' ? 'rtl' : 'ltr'}"`)) throw Error('Wrong document locale');
    const alternates = [...text.matchAll(/<link rel="alternate" hreflang="([^"]+)" href="([^"]+)"/g)];
    if (alternates.length !== 9 || new Set(alternates.map(row => row[1])).size !== 9 || ![...locales.map(l => l === 'zh' ? 'zh-Hans' : l), 'x-default'].every(lang => alternates.some(row => row[1] === lang))) throw Error('Incomplete hreflang');
    for (const row of text.matchAll(/<tr data-commercial[^>]*>[\s\S]*?<td[^>]*>(.*?)<\/td>/g)) if (row[1] !== dictionaries[locale].quote) throw Error('Localized commercial value');
    // Owner-authorized MOQ is admitted only in the three PDP contexts and four About ways.
    // Replace only validated values with quote text before the unchanged commercial checks.
    let commercialText = text;
    const expected = shopReadyMoq(locale).replaceAll('&', '&amp;');
    const card = /<article\b[^>]*data-product-card="[^"]+"[^>]*>[\s\S]*?<\/article>/g;
    const moq = /<p\b[^>]*class="shop-moq"[^>]*>([^<]*)<\/p>/g;
    const cards = [...commercialText.matchAll(card)];
    const listing = parts[0] === 'collections';
    if (listing) {
      const count = products.filter(p => parts.length === 2 || p.category === parts[1]).length;
      if (cards.length !== count) throw Error('Owner listing MOQ context changed');
    }
    for (const [tile] of cards) {
      const quantities = [...tile.matchAll(moq)];
      if (quantities.length !== 1 || quantities[0][1] !== expected) throw Error('Owner card MOQ changed');
      commercialText = commercialText.replace(tile, tile.replace(moq, `<p>${dictionaries[locale].quote}</p>`));
    }
    if (listing) {
      for (const [name,value] of [['ready', expected], ['custom', shopCustomMoq(locale).replaceAll('&','&amp;')]]) {
        const paragraph = new RegExp(`<p\\b[^>]*class="shop-owner-${name}"[^>]*>([^<]*)<\\/p>`, 'g');
        const matches = [...commercialText.matchAll(paragraph)];
        if (matches.length !== 1 || matches[0][1] !== value) throw Error(`Owner listing B2B MOQ changed ${path}/${name}: actual=${JSON.stringify(matches.map(m=>m[1]))} expected=${JSON.stringify(value)}`);
        commercialText = commercialText.replace(paragraph, `<p>${dictionaries[locale].quote}</p>`);
      }
    }
    if (parts[0] === 'products') {
      const expected = pdpMoq[locale].replaceAll('&', '&amp;'), expectedValue = pdpMoqValue[locale].replaceAll('&', '&amp;');
      const summary = /<p\b[^>]*class="moq-line"[^>]*>([^<]*)<\/p>/g;
      const summaries = [...commercialText.matchAll(summary)];
      if (summaries.length !== 1 || summaries[0][1] !== expected) throw Error('Owner MOQ summary changed');
      commercialText = commercialText.replace(summary, `<p>${dictionaries[locale].quote}</p>`);
      for (const scope of ['quick', 'full']) {
        const table = new RegExp(`<table\\b[^>]*data-${scope}-spec\\b[^>]*>[\\s\\S]*?<\\/table>`, 'g');
        const tables = [...commercialText.matchAll(table)];
        if (tables.length !== 1) throw Error('Owner MOQ spec context missing');
        let count = 0;
        const checked = tables[0][0].replace(/<tr\b[^>]*>\s*<th\b[^>]*>([^<]*)<\/th>\s*<td\b[^>]*>([^<]*)<\/td>\s*<\/tr>/g, (row, label, value) => {
          if (label !== dictionaries[locale].moq) return row;
          if (value !== expectedValue) throw Error('Owner MOQ spec changed');
          count++;
          return row.replace(value, dictionaries[locale].quote);
        });
        if (count !== 1) throw Error('Owner MOQ spec count changed');
        commercialText = commercialText.replace(tables[0][0], checked);
      }
    }
    if (parts.join('/') === 'about-us/index.html') {
      const list = /<ol\b[^>]*class="about-ways"[^>]*>[\s\S]*?<\/ol>/g;
      const lists = [...commercialText.matchAll(list)];
      if (lists.length !== 1) throw Error('Owner About ways context missing');
      let checked = lists[0][0];
      for (const way of aboutWays) {
        const paragraph = new RegExp(`<p\\b[^>]*data-owner-about-way="${way}"[^>]*>([^<]*)<\\/p>`, 'g');
        const matches = [...checked.matchAll(paragraph)];
        const expected = aboutMoqCopy(locale, way).replaceAll('&', '&amp;');
        if (matches.length !== 1 || matches[0][1] !== expected) throw Error('Owner About MOQ changed');
        checked = checked.replace(paragraph, `<p>${dictionaries[locale].quote}</p>`);
      }
      commercialText = commercialText.replace(lists[0][0], checked);
    }
    const plain = commercialText.replace(/<script\b[^>]*>[\s\S]*?<\/script>/gi, '').replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ');
    if (/(?:price|moq|lead time|sample cost)\s*:?\s*(?:[$€£¥]|\d)|(?:[$€£¥]|\b(?:USD|EUR|GBP|CNY))\s*\d/i.test(plain)) throw Error('Numeric commercial term');
    if (/(?:Pedido mínimo|Minimum de commande|Mindestbestellmenge|Минимальный заказ|الحد الأدنى للطلب|起订量)\s*:?\s*\d|\d+\s*件起订/iu.test(plain)) throw Error('Numeric localized MOQ outside owner context');
    for (const row of commercialText.matchAll(/<tr>\s*<th[^>]*>(Price|MOQ|Lead time|Sample cost)<\/th>\s*<td[^>]*>(.*?)<\/td>\s*<\/tr>/gi)) if (row[2] !== dictionaries[locale].quote) throw Error('Commercial table changed');
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

// HF1: removed per-image disclosure sentences must never reach any built locale page.
const removedImageCaptions = ["AI-generated illustration; actual details are subject to the confirmed sample.", "رسم توضيحي مولّد بالذكاء الاصطناعي؛ التفاصيل الفعلية وفق العينة المعتمدة.", "AI 生成示意图，实际细节以确认样品为准", "Ilustración generada por IA; los detalles reales se ajustarán a la muestra confirmada.", "Illustration générée par IA ; les détails réels dépendent de l’échantillon confirmé.", "KI-generierte Illustration; die tatsächlichen Details richten sich nach dem bestätigten Muster.", "Ilustração gerada por IA; os detalhes reais seguem a amostra confirmada.", "Иллюстрация создана ИИ; фактические детали определяются утверждённым образцом."];
for (const file of new Bun.Glob('**/*.html').scanSync(root)) {
 const html = await Bun.file(join(root, file)).text();
 for (const caption of removedImageCaptions) if (html.includes(caption)) throw Error(`HF1 removed caption in ${file}`);
}

// Per-image labels are prohibited as visible text; the one site footer disclosure remains.
for (const file of new Bun.Glob('**/*.html').scanSync(root)) {
 const html = await Bun.file(join(root, file)).text();
 const visible = html.replace(/<footer\b[\s\S]*?<\/footer>/gi, '').replace(/<(script|style)\b[\s\S]*?<\/\1>/gi, '').replace(/<[^>]*>/g, ' ').replace(/&#(?:39|x27);|&apos;/gi, "'").replace(/&amp;/g, '&');
 for (const locale of locales) {
  const t = dictionaries[locale];
  for (const label of [t['home.hero.15'], digitalLabel(locale), t.illustrativeNotice]) if (visible.includes(label)) throw Error(`HF1 removed image label in ${file}: ${label}`);
 }
}
