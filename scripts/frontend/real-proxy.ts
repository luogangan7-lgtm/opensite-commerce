/** CI-only transparent fault proxy. All /api/v1 requests reach the real Go app.
 * Control can hold or discard a completed response, never supply business DTOs.
 * Depends on the owned runtime capability; start-real-backend.py owns lifetime.
 */
import { readFileSync } from 'node:fs';
if (process.env.GITHUB_ACTIONS !== 'true') throw new Error('Real proxy requires CI');
const { control } = JSON.parse(readFileSync('.work/visual-real/runtime.json', 'utf8')) as { control: string };
type Fault = { path: string; method: string; mode: 'hold' | 'drop' | '503'; matched: boolean; status?: number; release: () => void; pending: Promise<void> };
let fault: Fault | undefined;

Bun.serve({ hostname: '127.0.0.1', port: 4181, idleTimeout: 30, async fetch(request) {
  const url = new URL(request.url);
  if (url.pathname === '/__visual/health') return new Response('ready');
  if (url.pathname.startsWith('/__visual/')) {
    if (request.headers.get('Authorization') !== `Bearer ${control}`) return new Response(null, { status: 403 });
    if (url.pathname === '/__visual/status') return Response.json({ matched: fault?.matched ?? false, status: fault?.status });
    if (url.pathname === '/__visual/release' && request.method === 'POST') {
      fault?.release(); fault = undefined; return new Response(null, { status: 204 });
    }
    if (url.pathname !== '/__visual/arm' || request.method !== 'POST' || fault) return new Response(null, { status: 409 });
    const input = await request.json() as { path?: string; method?: string; mode?: string };
    if (!input.path?.startsWith('/api/v1/') || !['GET', 'POST'].includes(input.method ?? '') || !['hold', 'drop', '503'].includes(input.mode ?? '')) return new Response(null, { status: 400 });
    let release!: () => void;
    const pending = new Promise<void>(resolve => { release = resolve; });
    fault = { path: input.path, method: input.method!, mode: input.mode as Fault['mode'], matched: false, release, pending };
    return new Response(null, { status: 204 });
  }
  if (!url.pathname.startsWith('/api/v1/') && url.pathname !== '/healthz') return new Response(null, { status: 404 });
  const current = fault && !fault.matched && fault.path === url.pathname && fault.method === request.method ? fault : undefined;
  // Claim before awaiting fetch: one armed fault can affect exactly one request.
  if (current) current.matched = true;
  const response = await fetch(`http://127.0.0.1:4180${url.pathname}${url.search}`, {
    method: request.method, headers: request.headers, redirect: 'manual',
    ...(request.method === 'GET' || request.method === 'HEAD' ? {} : { body: await request.arrayBuffer() }),
    signal: AbortSignal.timeout(10_000),
  });
  const bytes = await response.arrayBuffer();
  if (current) {
    current.status = response.status;
    if (current.mode === 'hold') {
      let timer: ReturnType<typeof setTimeout> | undefined;
      try { await Promise.race([current.pending, new Promise<void>((_, reject) => { timer = setTimeout(() => reject(new Error('Unreleased visual fault')), 15_000); })]); }
      finally { clearTimeout(timer); }
    } else {
      // Retain the matched upstream status until explicit release so the browser
      // can prove a dropped response was a committed 201, rather than a Go error.
      return new Response(null, { status: current.mode === '503' ? 503 : 502 });
    }
  }
  const headers = new Headers(response.headers);
  headers.delete('content-encoding'); headers.delete('content-length');
  return new Response(bytes, { status: response.status, headers });
} });
