/** Offline acceptance with independent mutations; no network, production writes or generated-image facts. */
import { test, expect } from 'bun:test';
import { cluster, validateCluster, sitemap, validateSitemap, factPaths, urlFor } from './contract';
const site = 'https://sourcyowear.example.invalid';
const paths = ['/', '/collections/', '/contact/', ...factPaths];
test('115 identities in eight reciprocal locale clusters; clean sitemap', () => {
  expect(new Set(factPaths).size).toBe(115);
  for (const path of paths) validateCluster(site, path, cluster(site, path));
  const urls = sitemap(site, paths);
  expect(urls.length).toBe(paths.length * 8);
  validateSitemap(site, paths, urls);
});
test('locale, canonical, reciprocity, zh-Hans and x-default mutations go red then restored green', () => {
  for (const mutate of [
    p => { p[0].locale = 'xx'; },
    p => { p[0].canonical += '?preview=1'; },
    p => { p[1].alternates.en = p[1].canonical; },
    p => { p[0].alternates.zh = p[0].alternates['zh-Hans']; delete p[0].alternates['zh-Hans']; },
    p => { delete p[0].alternates['x-default']; },
    p => { p.pop(); },
  ] as ((p: any[]) => void)[]) {
    const pages = cluster(site, factPaths[0]); mutate(pages);
    expect(() => validateCluster(site, factPaths[0], pages)).toThrow();
    validateCluster(site, factPaths[0], cluster(site, factPaths[0]));
  }
});
test('false facts and generated quality claims go red; clean identities green', () => {
  for (const extra of [{ material: 'UNKNOWN' }, { weight: '430g' }, { offers: { price: 1 } }, { aggregateRating: { ratingValue: 5 } }, { description: 'Generated image proves premium quality' }, { image: 'generated-detail.jpg' }]) {
    const pages = cluster(site, factPaths[0]);
    Object.assign(pages[0].structuredData as object, extra);
    expect(() => validateCluster(site, factPaths[0], pages)).toThrow('Unsupported product fact');
    validateCluster(site, factPaths[0], cluster(site, factPaths[0]));
  }
});
test('preview, query, errors, unknown routes/locales cannot enter sitemap', () => {
  for (const path of ['/preview/', '/404/', '/contact/?sent=1', '/about/', '/products/unknown/', '//evil.test/']) expect(() => sitemap(site, [path])).toThrow();
  for (const host of ['https://preview.example.test', 'https://app.pages.dev', 'http://localhost']) expect(() => sitemap(host, ['/'])).toThrow();
  expect(() => urlFor(site, '/', 'en-US' as any)).toThrow();
  const clean = sitemap(site, paths);
  expect(() => validateSitemap(site, paths, [...clean, 'https://app.pages.dev/preview/'])).toThrow();
  validateSitemap(site, paths, clean);
});
