/** Static apparel renderer; Astro CLI only; no tenant/backend runtime. */
import { createHash } from 'node:crypto';
import { copyFileSync, writeFileSync, readFileSync, readdirSync } from 'node:fs';
import { isProduction, buildOrigin } from './seo/mode.ts';
import { loadEnv } from 'vite';
import { measurementId, contentSecurityPolicy } from './analytics/config.mjs';
const gaId = measurementId(process.env.PUBLIC_GA4_ID ?? loadEnv(process.env.NODE_ENV || 'production', process.cwd(), 'PUBLIC_').PUBLIC_GA4_ID);
import { defineConfig } from 'astro/config';
// Static builds need only the public key; the Pages runtime enforces its secret binding.
const turnstile = Boolean(process.env.PUBLIC_TURNSTILE_SITE_KEY?.trim());
if (isProduction && !turnstile) throw Error('Turnstile public site key required; production must be protected');
export default defineConfig({
  output: 'static',
  site: buildOrigin,
  integrations: [{ name: 'seo-header-policy', hooks: { 'astro:build:done': ({ dir }) => {
    // Pages selects the nearest 404.html; _routes keeps static misses outside the RFQ Worker.
    for (const locale of ['zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar']) copyFileSync(new URL(`${locale}/404/index.html`, dir), new URL(`${locale}/404.html`, dir));
    writeFileSync(new URL('rfq-policy.json', dir), JSON.stringify({ required: isProduction || turnstile }));
    const file = new URL('_headers', dir);
    let headers = readFileSync(file, 'utf8');
    if (isProduction) headers = headers.replace(/^.*X-Robots-Tag:.*\n?/gmi, '');
    const hashes = new Set();
    for (const path of readdirSync(dir, { recursive: true }).filter(path => path.endsWith('.html'))) {
      const html = readFileSync(new URL(path, dir), 'utf8');
      for (const match of html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/g)) {
        if (match[2] && !match[1].includes('application/ld+json')) hashes.add(`'sha256-${createHash('sha256').update(match[2]).digest('base64')}'`);
      }
    }
    headers = headers.replace(/^.*Content-Security-Policy:.*\n?/gmi, '');
    writeFileSync(file, headers + '  Content-Security-Policy: ' + contentSecurityPolicy(gaId, [...hashes], turnstile) + '\n');
  } } }],
  trailingSlash: 'always',
  i18n: { locales: ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar'], defaultLocale: 'en', routing: { prefixDefaultLocale: false } },
});
