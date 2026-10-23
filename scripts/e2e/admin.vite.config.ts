/** Responsibility: serve the built admin with its real same-origin API in CI.
 * Dependencies: existing Vite/admin config; caller: CI runner preview command.
 * Invariants: no response mocks, no external proxy target, GitHub Actions only.
 */
import { defineConfig, mergeConfig, type Plugin } from 'vite';
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
  // All CI browsers share one proxy IP. Pace real requests below H4's fixed
  // 60/minute session admission; never disable or expand the production limit.
  plugins: [{ name: 'e2e-session-budget', configurePreviewServer(server) {
    let next = 0;
    server.middlewares.use('/api', (_request, response, nextMiddleware) => {
      const now = Date.now(), wait = Math.max(0, next - now);
      next = Math.max(now, next) + 1100;
      const timer = setTimeout(() => { if (!response.destroyed) nextMiddleware(); }, wait);
      response.on('close', () => clearTimeout(timer));
    });
  } } satisfies Plugin],
  preview: { proxy: { '/api': { target, changeOrigin: false, headers: { Origin: adminOrigin } } } },
}));
