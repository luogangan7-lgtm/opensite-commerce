/** Responsibility: official standalone Node SSR build with React islands.
 * Dependencies: Astro official integrations; callers: build/dev CLI.
 * Invariants: no prerendered tenant content or shared host cache; Node owns runtime SSR;
 * every SSR page gets a hash-based CSP header (no unsafe-inline, same-origin only).
 */
import { defineConfig } from 'astro/config';
import node from '@astrojs/node';
import react from '@astrojs/react';
import { readAllowedHosts } from './src/lib/host-policy';
const allowedHosts = readAllowedHosts(process.env.OSC_STOREFRONT_ALLOWED_HOSTS);
export default defineConfig({
  output: 'server', adapter: node({ mode: 'standalone' }), integrations: [react()],
  server: { host: '127.0.0.1', port: 4321 },
  vite: { define: { __OSC_STOREFRONT_ALLOWED_HOSTS__: JSON.stringify(allowedHosts), __OSC_COMMERCE_TEST_HTTP__: process.env.OPENSITE_ENVIRONMENT === 'test' && process.env.OSC_COMMERCE_TEST_HTTP === '1' } },
  security: {
    allowedDomains: allowedHosts.map(hostname => ({ hostname })),
    // Astro emits script-src/style-src 'self' plus SHA-256 hashes of the inline tags it renders.
    csp: { directives: ["default-src 'self'", "base-uri 'none'", "object-src 'none'", "frame-ancestors 'none'", "form-action 'self'"] },
  },
});
