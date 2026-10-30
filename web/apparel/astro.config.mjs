/** Static apparel renderer; Astro CLI only; no tenant/backend runtime. */
import { defineConfig } from 'astro/config';
export default defineConfig({
  output: 'static',
  site: 'https://feat-apparel-b2b-v1.sourcyo-apparel.pages.dev',
  trailingSlash: 'always',
  i18n: { locales: ['en', 'es', 'fr', 'de', 'pt', 'ru', 'ar'], defaultLocale: 'en', routing: { prefixDefaultLocale: false } },
});
