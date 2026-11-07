/** Static apparel renderer; Astro CLI only; no tenant/backend runtime. */
import { writeFileSync, readFileSync } from 'node:fs';
import { isProduction, buildOrigin } from './seo/mode.ts';
import { defineConfig } from 'astro/config';
export default defineConfig({
  output: 'static',
  site: buildOrigin,
  integrations: [{ name: 'seo-header-policy', hooks: { 'astro:build:done': ({ dir }) => {
    const file = new URL('_headers', dir);
    if (isProduction) writeFileSync(file, readFileSync(file, 'utf8').replace(/^.*X-Robots-Tag:.*\n?/gmi, ''));
  } } }],
  trailingSlash: 'always',
  i18n: { locales: ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar'], defaultLocale: 'en', routing: { prefixDefaultLocale: false } },
});
