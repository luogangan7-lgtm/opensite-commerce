/** Serves the shipped admin build and forwards its requests to the real fault proxy.
 * Caller: start-real-backend.py; dependencies: owned TLS files and built admin assets.
 * Invariants: CI only, loopback only, no DTOs/cookie fabrication, fixed upstream/origin.
 */
import { resolve, sep } from 'node:path';
if (process.env.GITHUB_ACTIONS !== 'true') throw new Error('Real admin requires CI');
const root = resolve('web/admin/dist');
const port = Number(process.env.VISUAL_ADMIN_PORT ?? '4350');
const upstream = process.env.VISUAL_ADMIN_UPSTREAM ?? 'http://127.0.0.1:4181';
if (![4350, 4352].includes(port) || !['http://127.0.0.1:4181', 'http://127.0.0.1:4185'].includes(upstream))
  throw new Error('Real admin requires an owned loopback lane');
let nextAdmission = 0;
Bun.serve({ hostname: '127.0.0.1', port, idleTimeout: 30,
  tls: { cert: Bun.file('.work/visual-real/localhost.pem'), key: Bun.file('.work/visual-real/localhost-key.pem') },
  async fetch(request) {
    const url = new URL(request.url);
    if (url.pathname.startsWith('/api/v1/')) {
      // Preserve H4's session admission budget across both viewport projects.
      const now = Date.now(), wait = Math.max(0, nextAdmission - now);
      nextAdmission = Math.max(now, nextAdmission) + 1100;
      if (wait) await Bun.sleep(wait);
      const headers = new Headers(request.headers);
      headers.set('Origin', 'https://admin.visual.test');
      return fetch(`${upstream}${url.pathname}${url.search}`, {
        method: request.method, headers, redirect: 'manual', signal: AbortSignal.timeout(25_000),
        ...(request.method === 'GET' || request.method === 'HEAD' ? {} : { body: await request.arrayBuffer() }),
      });
    }
    if (!['GET', 'HEAD'].includes(request.method)) return new Response(null, { status: 405 });
    const path = resolve(root, `.${decodeURIComponent(url.pathname)}`);
    if (!path.startsWith(root + sep) && path !== root) return new Response(null, { status: 404 });
    const file = Bun.file(path);
    if (path !== root && await file.exists()) return new Response(file);
    if (url.pathname.startsWith('/assets/')) return new Response(null, { status: 404 });
    return new Response(Bun.file(`${root}/index.html`));
  },
});
