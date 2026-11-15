/** Post-build acceptance of actual HTML/XML; exact fact allow-list and reciprocal locale clusters. */
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { cluster, validateCluster, validateSitemap } from '../seo/contract';
import { isProduction, placeholderOrigin } from '../seo/mode';
import { seoOrigin, publishedPaths } from '../seo/published';
import products from '../src/data/products.json';
import { locales } from '../seo/contract';
const root = process.env.APPAREL_DIST || 'web/apparel/dist';
const read = (path: string) => readFileSync(join(root, path), 'utf8');
const attrs = (tag: string) => Object.fromEntries([...tag.matchAll(/([\w:-]+)="([^"]*)"/g)].map(m => [m[1], m[2]]));
function head(html: string) { return html.match(/<head>([\s\S]*?)<\/head>/)?.[1] || ''; }
// All HTML, including routes excluded from the sitemap, must obey the indexing policy.
function files(path: string): string[] { return readdirSync(path, { withFileTypes: true }).flatMap(e => e.isDirectory() ? files(join(path, e.name)) : [join(path, e.name)]); }
let productAndCollectionPages = 0;
for (const file of files(root).filter(f => f.endsWith('.html'))) {
  const html = readFileSync(file, 'utf8');
  if (/(?:^|\/)(products|collections)\//.test(file)) {
    productAndCollectionPages++;
    const text = html.replace(/<script\b[^>]*>[\s\S]*?<\/script>/gi, '').replace(/<[^>]*>/g, ' ');
    if (/\b(?:TBD|placeholder|lorem ipsum)\b/i.test(text) || /\bTODO\b/.test(text)) throw Error(`Product placeholder: ${file}`);
  }
  const robots = [...head(html).matchAll(/<meta\b[^>]*>/g)].map(m => attrs(m[0])).filter(a => /^(robots|googlebot)$/i.test(a.name || ''));
  const noindex = robots.some(a => /\bnoindex\b/i.test(a.content || ''));
  if (isProduction ? noindex : !noindex) throw Error('HTML robots policy');
  if (isProduction && html.includes(placeholderOrigin)) throw Error('Production placeholder HTML');
}
if (productAndCollectionPages < products.length * locales.length) throw Error('Missing product/card output');
const headers = read('_headers');
const headerNoindex = /^\s*X-Robots-Tag:.*\bnoindex\b/im.test(headers);
const globalNoindex = /^\/\*\r?\n(?:(?:[ \t]+[^\n]*|[ \t]*)\r?\n)*?[ \t]+X-Robots-Tag:[^\n]*\bnoindex\b/im.test(headers);
if (isProduction ? headerNoindex : !globalNoindex) throw Error('X-Robots-Tag policy');
let count = 0;
for (const path of publishedPaths) {
  const pages = cluster(seoOrigin, path).map(expected => {
    const pathname = new URL(expected.canonical).pathname;
    const html = read(join(pathname.slice(1), 'index.html'));
    const h = head(html);
    const links = [...h.matchAll(/<link\b[^>]*>/g)].map(m => attrs(m[0]));
    const canonical = links.filter(a => a.rel === 'canonical');
    if (canonical.length !== 1) throw Error('Canonical count');
    const alternatives = links.filter(a => a.rel === 'alternate');
    if (alternatives.length !== 9) throw Error('Alternate count');
    const scripts = [...h.matchAll(/<script type="application\/ld\+json">([\s\S]*?)<\/script>/g)];
    if (scripts.length !== (expected.structuredData ? 1 : 0)) throw Error('Structured data count');
    const meta = [...h.matchAll(/<meta\b[^>]*>/g)].map(m => attrs(m[0]));
    for (const key of ['title', 'description', 'type', 'url', 'site_name']) {
      const values = meta.filter(a => a.property === `og:${key}`);
      if (values.length !== 1 || !values[0].content || /UNKNOWN/.test(values[0].content)) throw Error(`Missing OG ${key}`);
      if (key === 'url' && values[0].content !== expected.canonical) throw Error('OG URL');
    }
    count++;
    return { ...expected, canonical: canonical[0].href, alternates: Object.fromEntries(alternatives.map(a => [a.hreflang, a.href])), structuredData: scripts.length ? JSON.parse(scripts[0][1]) : null };
  });
  validateCluster(seoOrigin, path, pages);
}
const xml = read('sitemap.xml');
if (!xml.startsWith('<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">') || !xml.endsWith('</urlset>')) throw Error('Invalid XML envelope');
const urls = [...xml.matchAll(/<url><loc>([^<]+)<\/loc><\/url>/g)].map(m => m[1]);
if (xml.replace(/^.*?<urlset[^>]*>/, '').replace('</urlset>', '').replace(/<url><loc>[^<]+<\/loc><\/url>/g, '') !== '') throw Error('Unexpected XML fields');
validateSitemap(seoOrigin, publishedPaths, urls);
if (/UNKNOWN|pages\.dev|preview/i.test(xml)) throw Error('Unsafe sitemap');
if (read('robots.txt') !== `User-agent: *\nAllow: /\nSitemap: ${seoOrigin}/sitemap.xml\n`) throw Error('Crawler policy');
console.log(`PASS: ${count} real HTML pages; reciprocal canonical/hreflang, OG, exact JSON-LD; ${urls.length} XML URLs; crawler access to noindex policy.`);

// Release acceptance uses the actual bytes, including the compiled worker and all locale RFQs.
if (isProduction) {
  for (const file of files(root)) {
    if (readFileSync(file).includes(Buffer.from('example.invalid'))) throw Error(`Production example.invalid: ${file}`);
  }
  const siteKey = process.env.PUBLIC_TURNSTILE_SITE_KEY?.trim();
  if (!siteKey) throw Error('Production public Turnstile site key missing');
  const policies = [...headers.matchAll(/^\s*Content-Security-Policy:\s*(.+)$/gmi)].map(m => m[1]);
  // The generated /* policy covers every locale, including every page containing an RFQ.
  if (!/^\/\*\r?\n(?:[ \t]+[^\n]*\r?\n)*[ \t]+Content-Security-Policy:/m.test(headers) || policies.length !== 1) throw Error('Production global CSP missing');
  for (const directive of ['script-src', 'frame-src']) {
    const sources = policies[0].split(';').map(s => s.trim()).find(s => s.startsWith(`${directive} `))?.split(/\s+/).slice(1);
    if (!sources?.includes('https://challenges.cloudflare.com')) throw Error(`Production Turnstile CSP ${directive}`);
  }
  for (const locale of locales) {
    const html = read(join(locale === 'en' ? '' : locale, 'contact/index.html'));
    const widgets = [...html.matchAll(/<div\b[^>]*>/g)].map(m => attrs(m[0])).filter(a => a.class?.split(/\s+/).includes('cf-turnstile'));
    if (widgets.length !== 1 || widgets[0]['data-sitekey'] !== siteKey || !html.includes('src="https://challenges.cloudflare.com/turnstile/v0/api.js"')) throw Error(`Production Turnstile widget: ${locale}`);
  }
  console.log('PASS: production dist has no example.invalid; eight contact widgets use the public key; global CSP allows Turnstile script and frame.');
}
