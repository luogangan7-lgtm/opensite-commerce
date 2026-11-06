/** SEO fixture contract. Depends on pinned R04 identity records; consumed by offline gates.
 * Invariants: eight reciprocal locales; no preview indexing; no inferred product claims.
 * Integration into rendered pages requires a separate release gate.
 */
import facts from './sku-source-delta.json';
export const locales = ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar'] as const;
export type Locale = typeof locales[number];
export interface Page { locale: Locale; path: string; canonical: string; alternates: Record<string, string>; structuredData: unknown }
function origin(value: string): string {
  const url = new URL(value);
  if (url.protocol !== 'https:' || url.username || url.password || url.pathname !== '/' || url.search || url.hash || /(?:pages\.dev|localhost|preview)/i.test(url.hostname)) throw Error('Non-public origin');
  return url.origin;
}
function route(path: string): string {
  // Deliberately closed: category taxonomy and unfinished editorial pages need separate approval.
  if (path !== '/' && path !== '/collections/' && path !== '/contact/' && !facts.some(f => path === `/products/${f.slug}/`)) throw Error('Unapproved route');
  return path;
}
export function urlFor(site: string, path: string, locale: Locale): string {
  if (!(locales as readonly string[]).includes(locale)) throw Error('Invalid locale');
  return origin(site) + (locale === 'en' ? '' : `/${locale}`) + route(path);
}
export function productData(site: string, path: string, locale: Locale): unknown {
  const canonical = urlFor(site, path, locale);
  const fact = facts.find(f => path === `/products/${f.slug}/`);
  // Identity is corroborated by catalog reference; UNKNOWN fabric/unit/weight are never emitted.
  return fact ? { '@context': 'https://schema.org', '@type': 'Product', identifier: fact.slug, url: canonical } : null;
}
export function page(site: string, path: string, locale: Locale): Page {
  const alternates = Object.fromEntries(locales.map(l => [l === 'zh' ? 'zh-Hans' : l, urlFor(site, path, l)]));
  alternates['x-default'] = urlFor(site, path, 'en');
  return { locale, path, canonical: urlFor(site, path, locale), alternates, structuredData: productData(site, path, locale) };
}
export function cluster(site: string, path: string): Page[] { return locales.map(l => page(site, path, l)); }
export function validateCluster(site: string, path: string, pages: Page[]): void {
  if (pages.length !== 8 || new Set(pages.map(p => p.locale)).size !== 8) throw Error('Incomplete locale cluster');
  for (const actual of pages) {
    const expected = page(site, path, actual.locale);
    if (actual.path !== path || actual.canonical !== expected.canonical) throw Error('Wrong canonical/path');
    if (JSON.stringify(Object.entries(actual.alternates).sort()) !== JSON.stringify(Object.entries(expected.alternates).sort())) throw Error('Wrong reciprocal alternates');
    // Exact allow-list: adding offers, material, rating, generated quality prose or UNKNOWN fails.
    if (JSON.stringify(actual.structuredData) !== JSON.stringify(expected.structuredData)) throw Error('Unsupported product fact');
  }
}
export function sitemap(site: string, paths: string[]): string[] {
  return [...new Set(paths.flatMap(path => locales.map(locale => urlFor(site, path, locale))))].sort();
}
export function validateSitemap(site: string, paths: string[], urls: string[]): void {
  if (JSON.stringify([...urls].sort()) !== JSON.stringify(sitemap(site, paths))) throw Error('Wrong sitemap inventory');
}
export const factPaths = facts.map(f => `/products/${f.slug}/`);
