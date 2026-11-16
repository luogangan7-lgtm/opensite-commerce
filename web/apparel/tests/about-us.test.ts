/** CP-ABOUT public route contract; independent route/locale expectations, no renderer internals. */
import { test, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { cluster, sitemap } from '../seo/contract';
import { publishedPaths } from '../seo/published';

test('approved brand story has eight reciprocal SEO routes and sitemap entries', () => {
  expect(publishedPaths).toContain('/about-us/');
  const pages = cluster('https://example.org', '/about-us/');
  expect(pages).toHaveLength(8);
  expect(pages.find(p => p.locale === 'ar')?.canonical).toBe('https://example.org/ar/about-us/');
  for (const page of pages) expect(Object.keys(page.alternates)).toHaveLength(9);
  expect(sitemap('https://example.org', ['/about-us/'])).toHaveLength(8);
});

test('Factory and brand story remain separate static page kinds', () => {
  const routes = readFileSync(new URL('../src/pages/[...path].astro', import.meta.url), 'utf8');
  expect(routes).toContain("kind === 'about'");
  expect(routes).toContain("kind === 'about-us'");
});
