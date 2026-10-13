/** Responsibility: serve the built admin with its real same-origin API in CI.
 * Dependencies: existing Vite/admin config; caller: CI runner preview command.
 * Invariants: no response mocks, no external proxy target, GitHub Actions only.
 */
import { defineConfig, mergeConfig } from 'vite';
import admin from '../../web/admin/vite.config';

if (process.env.CI !== 'true' || process.env.GITHUB_ACTIONS !== 'true') {
  throw new Error('Admin E2E preview requires GitHub Actions');
}
const target = process.env.E2E_ADMIN_PROXY_TARGET ?? process.env.E2E_API_URL;
if (!['http://api:8080', 'http://127.0.0.1:8080'].includes(target ?? '')) {
  throw new Error('Admin E2E API must use its owned Compose or loopback service');
}

// Staff login only accepts an exact HTTPS Origin, but this disposable stack is plain
// loopback HTTP: the proxy presents the configured HTTPS admin origin to the real API.
const adminOrigin = process.env.E2E_ADMIN_ORIGIN ?? '';
if (!/^https:\/\/[a-z0-9.-]+$/.test(adminOrigin)) throw new Error('E2E_ADMIN_ORIGIN must be an HTTPS origin');

export default defineConfig(mergeConfig(admin, {
  preview: { proxy: { '/api': { target, changeOrigin: false, headers: { Origin: adminOrigin } } } },
}));
