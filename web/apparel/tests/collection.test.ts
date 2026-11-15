/** CP6 catalog and facet contract; derives counts from the public data. */
import { test, expect } from 'bun:test';
import { categories, products } from '../src/data/catalog';
import { matchesFilters } from '../src/data/collection-filters';
import { publishedPaths } from '../seo/published';
import { cluster } from '../seo/contract';
test('zip jackets own exactly K12–K19 and every product belongs to one category', () => {
  expect(categories['zip-ups-jackets']).toBe('Zip-ups & Jackets');
  const moved = products.filter(p => /^hoodie-set-k(?:12|13|14|15|16|17|18|19)-/.test(p.slug));
  expect(moved).toHaveLength(10);
  for (const p of moved) expect(p.category).toBe('zip-ups-jackets');
  expect(products.every(p => Object.hasOwn(categories,p.category))).toBe(true);
  expect(new Set(products.map(p=>p.slug)).size).toBe(products.length);
  expect(Object.keys(categories).reduce((n,c)=>n+products.filter(p=>p.category===c).length,0)).toBe(products.length);
});
test('category SEO publishes reciprocal eight-locale routes', () => {
  for(const c of Object.keys(categories)) {
    const path=`/collections/${c}/`;
    expect(publishedPaths).toContain(path);
    expect(cluster('https://example.org',path)).toHaveLength(8);
  }
});
test('filters intersect lining and weight and never guess unknown weight', () => {
  const crew=products.find(p=>p.slug==='crew-fleece-8900-31')!;
  const heavy=products.find(p=>p.slug==='cotton430-hoodie-70046')!;
  expect(matchesFilters(crew,new URLSearchParams('lining=fleece'))).toBe(true);
  expect(matchesFilters(crew,new URLSearchParams('weight=heavy'))).toBe(false);
  expect(matchesFilters(heavy,new URLSearchParams('lining=fleece&weight=heavy'))).toBe(true);
  expect(matchesFilters(heavy,new URLSearchParams('lining=unlined&weight=heavy'))).toBe(false);
  expect(matchesFilters(crew,new URLSearchParams())).toBe(true);
});
