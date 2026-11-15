/** CP2 frozen Brand and shell contract; FL1 native asset bytes are the reference. */
import { test, expect } from 'bun:test';
import { readFileSync, existsSync } from 'node:fs';
const read = (p: string) => readFileSync(new URL('../' + p, import.meta.url), 'utf8');
test('shell uses a reusable outlined brand and four footer columns', () => {
 const shell = read('src/layouts/Shell.astro');
 expect(shell).toContain('<BrandLogo');
 expect(shell).toContain('footer-legal');
 expect(shell).toContain('t.footerHelp');
 expect(shell).toContain('header-over-image');
});
test('footer links only point at routes that exist (no size-guide page; size chart lives on each PDP)', () => {
 expect(read('src/layouts/Shell.astro')).not.toContain('/size-guide/');
});
test('cookie overlay shares one treatment for both consent choices', () => {
 const cookie = read('src/components/CookieConsent.astro');
 expect(cookie).toContain('position: fixed');
 expect(cookie).toContain('var(--sy-color-surface)');
 expect(cookie).not.toContain('#f4f2ee');
});
test('native favicon sizes and manifest are shipped', () => {
 for (const name of ['icon-16.png','icon-32.png','icon-180.png','site.webmanifest']) expect(existsSync(new URL('../public/brand/' + name, import.meta.url))).toBe(true);
});
