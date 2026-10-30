/** Preview-only live imagery receipts. Browser execution is confined to GitHub CI. */
import { test, expect } from '@playwright/test';
import { assertSlotReferences } from '../image-plan';
import { mkdirSync, writeFileSync, readFileSync } from 'node:fs';
test.skip(!process.env.APPAREL_PREVIEW_URL, 'Requires the immutable, packaged Pages preview');
test('deployed slots, manifest, LCP budget, locale routes and image network allow-list', async ({ page, request }) => {
  test.setTimeout(120_000);
  const response = await request.get('/deploy-manifest.json');
  expect(response.status()).toBe(200);
  const manifest = await response.json();
  expect(manifest.sha).toBe(process.env.GITHUB_SHA);
  const reviewed = manifest.slotReviews as { id: string; pass: boolean }[];
  const templateIds = Object.keys(JSON.parse(readFileSync('web/apparel/src/i18n/en.json', 'utf8'))).filter(key => key.startsWith('alt.')).map(key => key.slice(4).replace('.', '/'));
  expect(reviewed.map(row => row.id).sort()).toEqual(templateIds.sort());
  expect(reviewed.every(row => typeof row.pass === 'boolean')).toBe(true);
  expect(manifest.slots.map((row: { id: string }) => row.id).sort()).toEqual(reviewed.filter(row => row.pass).map(row => row.id).sort());
  expect(JSON.stringify(manifest)).not.toMatch(/dh-legacy|model_front\.png|\/hd\//);
  for (const slot of manifest.slots) expect(slot.rights).toBe('ai-generated-illustrative');
  const target: Record<string, string> = { home: '/', collection: '/collections/', about: '/about/', oem: '/about/', pdp: '/products/crew-fleece-8900-32/', contact: '/contact/' };
  for (const [kind, route] of Object.entries(target)) {
    const html = await (await request.get(route)).text();
    assertSlotReferences(html, manifest.slots, kind);
    for (const slot of manifest.slots.filter((s: { id: string }) => s.id.startsWith(kind + '/'))) {
      expect(html).toContain(`data-slot="${slot.id}"`);
      expect(html).toContain(`width="${slot.w}" height="${slot.h}"`);
    }
  }
  await page.goto('/contact/');
  const banner = page.locator('.contact-banner');
  await expect(banner).toBeVisible();
  const contactPass = reviewed.find(row => row.id === 'contact/banner')!.pass;
  await expect(banner.locator('img')).toHaveCount(contactPass ? 1 : 0);
  const contactImages = await banner.locator('img').count();
  const cream = await banner.evaluate(node => ({ actual: getComputedStyle(node).backgroundColor, token: getComputedStyle(node).getPropertyValue('--sy-color-placeholder').trim(), height: node.getBoundingClientRect().height }));
  expect(cream.actual).toBe('rgb(231, 226, 214)');
  expect(cream.token).toBe('#e7e2d6');
  expect(cream.height).toBeGreaterThan(0);
  const hero = manifest.files.find((file: { url: string }) => file.url === '/img/site/illustrative_home_hero-1024.avif');
  await page.setViewportSize({ width: 1024, height: 844 });
  await page.goto('/');
  await expect.poll(() => page.locator('[data-slot="home/hero"] img').evaluate(node => new URL((node as HTMLImageElement).currentSrc).pathname)).toBe(hero.url);
  const heroResponse = await request.get(hero.url), heroBytes = (await heroResponse.body()).length;
  expect(heroBytes).toBe(hero.bytes); expect(heroBytes).toBeLessThan(300_000);
  await page.setViewportSize({ width: 390, height: 844 });
  const network: Record<string, string[]> = {};
  for (const route of ['/', '/es/', '/collections/crew-fleece/', '/products/crew-fleece-8900-33/']) {
    const requests = new Set<string>();
    const listener = (req: import('@playwright/test').Request) => { requests.add(new URL(req.url()).pathname); };
    page.on('request', listener);
    expect((await page.goto(route))?.status()).toBe(200);
    for (const image of await page.locator('img').all()) await image.scrollIntoViewIfNeeded();
    await expect.poll(() => page.locator('img').evaluateAll(nodes => nodes.every(node => (node as HTMLImageElement).complete && (node as HTMLImageElement).naturalWidth > 0))).toBe(true);
    page.off('request', listener);
    network[route] = [...requests].sort();
    expect(network[route].some(path => path.includes('/hd/'))).toBe(false);
    for (const path of network[route].filter(path => path.startsWith('/img/'))) expect(manifest.files.some((file: { url: string }) => file.url === path)).toBe(true);
  }
  const statuses: Record<string, number> = {};
  for (const locale of ['en', 'es', 'fr', 'de', 'pt', 'ru', 'ar']) for (const route of ['/', '/collections/crew-fleece/', '/products/crew-fleece-8900-33/', '/contact/']) {
    const path = `${locale === 'en' ? '' : `/${locale}`}${route}`, response = await request.get(path);
    statuses[path] = response.status(); expect(response.status()).toBe(200);
  }
  for (const route of ['/', '/collections/crew-fleece/', '/products/crew-fleece-8900-33/']) {
    await page.goto('/ar' + route);
    await expect(page.locator('html')).toHaveAttribute('dir', 'rtl');
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  }
  const result = { contact: { published: contactPass, images: contactImages, cream }, statuses, arabic390: 'home/collection/PDP rtl and no horizontal scroll', sha: manifest.sha, slots: manifest.slots.length, packshots: manifest.packshots.map((p: { slug: string }) => p.slug), hero1024: { file: hero.url, bytes: heroBytes }, network };
  mkdirSync('evidence', { recursive: true }); writeFileSync('evidence/live-imagery.json', JSON.stringify(result, null, 2));
  console.log('LIVE IMAGERY', JSON.stringify(result));
});
