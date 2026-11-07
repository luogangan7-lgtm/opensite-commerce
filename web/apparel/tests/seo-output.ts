/** Post-build acceptance of actual HTML/XML; exact fact allow-list and reciprocal locale clusters. */
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { cluster, validateCluster, validateSitemap } from '../seo/contract';
import { seoOrigin, publishedPaths } from '../seo/published';
const root = process.env.APPAREL_DIST || 'web/apparel/dist';
const read = (path: string) => readFileSync(join(root, path), 'utf8');
const attrs = (tag: string) => Object.fromEntries([...tag.matchAll(/([\w:-]+)="([^"]*)"/g)].map(m => [m[1], m[2]]));
function head(html: string) { return html.match(/<head>([\s\S]*?)<\/head>/)?.[1] || ''; }
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
if (read('robots.txt') !== `User-agent: *\nDisallow: /\nSitemap: ${seoOrigin}/sitemap.xml\n`) throw Error('Crawler policy');
console.log(`PASS: ${count} real HTML pages; reciprocal canonical/hreflang, OG, exact JSON-LD; ${urls.length} XML URLs; robots pre-release block.`);
