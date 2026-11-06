/** Independent output matrix: eight locales × every catalog/static route, including 404. */
import { readFileSync, existsSync } from 'node:fs';
import { join } from 'node:path';
import { products, categories } from '../src/data/catalog';
const root = process.env.APPAREL_DIST || 'web/apparel/dist';
const languages = ['en','zh','es','fr','de','pt','ru','ar'];
const routes = ['', 'about', 'contact', 'collections', 'oem-odm', 'sampling', 'shipping', 'privacy', 'terms', '404', ...Object.keys(categories).map(c => `collections/${c}`), ...products.map(p => `products/${p.slug}`)];
let checked = 0;
for (const locale of languages) for (const route of routes) {
  const prefix = locale === 'en' ? '' : locale;
  const file = join(root, prefix, route === '404' && locale === 'en' ? '404.html' : `${route}/index.html`);
  if (!existsSync(file)) throw Error(`Missing route ${locale}/${route}`);
  const html = readFileSync(file, 'utf8');
  if (!html.includes(`lang="${locale}" dir="${locale === 'ar' ? 'rtl' : 'ltr'}"`)) throw Error(`Locale/RTL ${file}`);
  const pathname = '/' + [prefix,route].filter(Boolean).join('/') + (prefix || route ? '/' : '');
  for (const language of languages) {
    const hreflang = language === 'zh' ? 'zh-Hans' : language;
    const target = '/' + [language === 'en' ? '' : language,route].filter(Boolean).join('/') + (language !== 'en' || route ? '/' : '');
    if (!html.includes(`hreflang="${hreflang}" href="https://feat-apparel-b2b-v1.sourcyo-apparel.pages.dev${target}"`)) throw Error(`Alternate ${pathname} ${hreflang}`);
  }
  const english = '/' + route + (route ? '/' : '');
  if (!html.includes(`hreflang="x-default" href="https://feat-apparel-b2b-v1.sourcyo-apparel.pages.dev${english}"`)) throw Error(`x-default ${file}`);
  checked++;
}
console.log(`PASS: ${checked} generated routes (${languages.length} × ${routes.length}); zh-Hans, EN x-default, AR RTL. Contact sent/query states use the same static page and browser gate.`);
