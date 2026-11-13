/** Abuse boundary gate: synthetic cache, Siteverify and storage; no live credentials. */
import { afterEach, test, expect, mock, spyOn } from 'bun:test';
import { onRequestPost } from '../functions/api/rfq';
const env = { SUPABASE_URL: 'https://database.example.invalid', SUPABASE_SECRET_KEY: 'synthetic', TURNSTILE_SECRET_KEY: 'synthetic' };
const body = { name: 'CI', company: 'CI', email: 'ci@example.invalid', message: 'Synthetic inquiry', 'cf-turnstile-response': 'synthetic-token' };
const originalCaches = Object.getOwnPropertyDescriptor(globalThis, 'caches');
afterEach(() => { mock.restore(); if (originalCaches) Object.defineProperty(globalThis, 'caches', originalCaches); else Reflect.deleteProperty(globalThis, 'caches'); });
function setup(success = true) {
  const entries = new Map<string, Response>();
  Object.defineProperty(globalThis, 'caches', { configurable: true, value: { default: { match: async (r: Request) => entries.get(r.url)?.clone(), put: async (r: Request, v: Response) => { entries.set(r.url, v.clone()); } } } });
  const calls: Request[] = [];
  spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
    const r = new Request(input, init); calls.push(r);
    return r.url.includes('siteverify') ? Response.json({ success, hostname: 'example.invalid', action: 'rfq' }) : Response.json([{ id: '524f5000-0000-4000-8000-000000000001' }], { status: 201 });
  });
  const run = (value = body, ip = '192.0.2.1', bindings = env) => onRequestPost({ request: new Request('https://example.invalid/api/rfq', { method: 'POST', headers: { 'CF-Connecting-IP': ip }, body: JSON.stringify(value) }), env: bindings });
  return { run, calls };
}
test('Turnstile rejection returns private 403 before storage', async () => {
  const f = setup(false); const r = await f.run(); expect(r.status).toBe(403); expect(await r.json()).toEqual({ error: 'error.challenge' }); expect(f.calls).toHaveLength(1);
});
test('sixth attempt per IP in ten minutes returns 429; other IP remains usable', async () => {
  const f = setup(); for (let i = 0; i < 5; i++) expect((await f.run()).status).toBeLessThan(400);
  const r = await f.run(); expect(r.status).toBe(429); expect(r.headers.get('Retry-After')).toBeTruthy(); expect((await f.run(body, '192.0.2.2')).status).toBe(204);
});
test('verified normal path returns empty 204 and private token never reaches storage', async () => {
  const f = setup(); const r = await f.run(); expect(r.status).toBe(204); expect(await r.text()).toBe('');
  const verification = f.calls[0]; expect(verification.url).toBe('https://challenges.cloudflare.com/turnstile/v0/siteverify'); expect(verification.redirect).toBe('manual');
  const payload = await verification.json(); expect(payload.secret).toBe(env.TURNSTILE_SECRET_KEY); expect(payload.remoteip).toBe('192.0.2.1'); expect(payload.response).toBe('synthetic-token');
  expect(await f.calls[1].json()).not.toHaveProperty('cf-turnstile-response');
});
test('production build without Turnstile keys is rejected by real Astro config', () => {
  const r = Bun.spawnSync(['bun', 'run', '--cwd', 'web/apparel', 'build'], { env: { ...process.env, SEO_BUILD_MODE: 'production', PRODUCTION_ORIGIN: 'https://sourcyo.com', PUBLIC_TURNSTILE_SITE_KEY: '', TURNSTILE_SECRET_KEY: '' } });
  expect(r.exitCode).not.toBe(0); expect(r.stderr.toString() + r.stdout.toString()).toContain('Turnstile');
// Astro CLI startup can exceed Bun's 5 s default on a contended CI runner.
}, 60_000);
test('missing, oversized, expired and replay-rejected tokens do not reach storage', async () => {
  const f = setup(false);
  for (const token of ['', 'x'.repeat(2049), 'expired', 'replayed']) {
    const r = await f.run({ ...body, 'cf-turnstile-response': token }); expect(r.status).toBe(403);
  }
  expect(f.calls.every(r => r.url.includes('siteverify'))).toBe(true);
});
test('production policy without runtime secret fails closed', async () => {
  const f = setup();
  const r = await f.run(body, '192.0.2.1', { ...env, TURNSTILE_SECRET_KEY: '', ASSETS: { fetch: async () => Response.json({ required: true }) } } as typeof env);
  expect(r.status).toBe(503); expect(f.calls).toHaveLength(0);
});
test('cache failure fails closed; expired window becomes usable again', async () => {
  const f = setup();
  for (let i = 0; i < 5; i++) await f.run();
  expect((await f.run()).status).toBe(429);
  const now = Date.now(); spyOn(Date, 'now').mockReturnValue(now + 600001);
  expect((await f.run()).status).toBe(204);
  Object.defineProperty(globalThis, 'caches', { configurable: true, value: { default: { match: async () => { throw Error('cache'); } } } });
  expect((await f.run()).status).toBe(503);
});
test('Siteverify network failure stays private and prevents storage', async () => {
  const f = setup(); spyOn(globalThis, 'fetch').mockRejectedValue(new Error('private provider detail'));
  const r = await f.run(); expect(r.status).toBe(403); expect(await r.text()).not.toContain('private'); expect(f.calls).toHaveLength(0);
});
test('security feedback exists for all eight locales', async () => {
  const { abuseMessages } = await import('../src/data/abuse-i18n');
  expect(Object.keys(abuseMessages)).toHaveLength(8);
  for (const messages of Object.values(abuseMessages)) for (const key of ['error.challenge', 'error.rate'] as const) expect(messages[key].length).toBeGreaterThan(10);
});
test('necessary Turnstile CSP stays independent of optional analytics', async () => {
  const { contentSecurityPolicy } = await import('../analytics/config.mjs');
  for (const ga of ['', 'G-CONSENTTEST']) for (const necessary of [false, true]) {
    const csp = contentSecurityPolicy(ga, [], necessary);
    for (const directive of ['script-src', 'connect-src', 'frame-src']) {
      const value = csp.split(';').find((part: string) => part.trim().startsWith(directive + ' ')) || '';
      expect(value.includes('https://challenges.cloudflare.com')).toBe(necessary);
    }
    expect(csp.includes('googletagmanager.com')).toBe(Boolean(ga));
  }
});
