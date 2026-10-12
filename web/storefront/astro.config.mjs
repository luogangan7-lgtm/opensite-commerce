/** Responsibility: official standalone Node SSR build with React islands.
 * Dependencies: Astro official integrations; callers: build/dev CLI.
 * Invariants: no prerendered tenant content or shared host cache; Node owns runtime SSR.
 */
import { defineConfig } from 'astro/config';
import node from '@astrojs/node';
import react from '@astrojs/react';
import { readAllowedHosts } from './src/lib/host-policy';
export default defineConfig({
  output: 'server', adapter: node({ mode: 'standalone' }), integrations: [react()],
  server: { host: '127.0.0.1', port: 4321 },
  security: { allowedDomains: readAllowedHosts(process.env.OSC_STOREFRONT_ALLOWED_HOSTS).map(hostname => ({ hostname })) },
});
