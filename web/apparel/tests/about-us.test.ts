/** CP-ABOUT public route contract; independent route/locale expectations, no renderer internals. */
import { test, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { cluster, sitemap } from '../seo/contract';
import { publishedPaths } from '../seo/published';
import { dictionaries, locales } from '../src/i18n';
import { aboutWays, aboutMoqCopy } from '../src/data/owner-facts';
import { digitalDelivery } from '../image-plan';
import media from '../about-media.json';

test('About media are eleven SHA-bound masters with only packaged 1024/640 delivery paths', () => {
  expect(media).toHaveLength(11);
  expect(new Set(media.map(image => image.src)).size).toBe(media.length);
  for (const image of media) {
    expect(image.sha256).toMatch(/^[a-f0-9]{64}$/);
    expect(image.source.startsWith('/') || image.source.split('/').includes('..')).toBe(false);
    expect(image.src).toStartWith('/img/about/');
    expect(digitalDelivery(image).map(file => file.width)).toEqual([1024, 640]);
    expect(digitalDelivery(image).every(file => file.sourceSha256 === image.sha256)).toBe(true);
  }
});

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

test('About ways use the owner-approved 100/100/100/1000 quantities in all eight locales', () => {
  for (const locale of locales) for (const way of aboutWays) {
    const template = dictionaries[locale][`about.ways-body-${way}`];
    expect(template.match(/\{moq\}/g)).toHaveLength(1);
    expect(template).not.toMatch(/\d/);
    const amounts = [...aboutMoqCopy(locale, way).matchAll(/\d(?:[\d,. ]*\d)?/g)].map(row => Number(row[0].replace(/\D/g, '')));
    expect(amounts).toEqual([way === '04' ? 1000 : 50]);
  }
});
