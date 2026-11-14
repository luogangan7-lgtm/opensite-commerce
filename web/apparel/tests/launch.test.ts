/** Launch regression: catalog is the independent public inventory; no private assets or network. */
import { test, expect } from 'bun:test';
import products from '../src/data/products.json';
import { factPaths, productData, sitemap } from '../seo/contract';
import { publishedPaths } from '../seo/published';
import { GET } from '../src/pages/robots.txt';
import digital from '../digital-sources.json';
test('all 115 approved public identities have eight SEO routes and Product data', () => {
  expect(products.length).toBe(115);
  const paths = products.map(p => `/products/${p.slug}/`);
  expect(new Set(factPaths).size).toBe(factPaths.length);
  for (const path of paths) {
    expect(factPaths).toContain(path);
    expect(publishedPaths).toContain(path);
    expect(productData('https://example.org', path, 'ar')).toMatchObject({ '@type': 'Product', url: `https://example.org/ar${path}` });
  }
  expect(sitemap('https://example.org', publishedPaths).length).toBe(984);
  expect(() => productData('https://example.org', '/products/unknown/', 'en')).toThrow();
  expect(() => productData('https://example.org', '/products/aston-hoodie-69012/', 'en')).toThrow();
});
test('public preview permits fetching its noindex signal', async () => {
  expect(await GET().text()).toContain('Allow: /');
  expect(await GET().text()).not.toContain('Disallow: /');
});
test('single image release points at its independent PASS receipt', () => {
  expect(digital.find(r => r.key === 'crew-fleece-8900-46__model-03')?.acceptance).toBe('fleet-20261104/r5-one-visual-20261107/REPORT.md');
});
