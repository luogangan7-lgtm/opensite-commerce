/** RFQ POST boundary; Pages uses private Supabase bindings; DB owns id/time, no PII logging. */
type Context = { request: Request; env: { SUPABASE_URL: string; SUPABASE_SECRET_KEY: string; TURNSTILE_SECRET_KEY?: string; ASSETS?: { fetch(request: Request): Promise<Response> | Response } } };
export const RFQ_IP_LIMIT = 5;
export const RFQ_WINDOW_SECONDS = 600;
// ponytail: Cache API counters are per-colo and non-atomic; Turnstile remains the primary abuse barrier.
async function abuseCheck(request: Request, env: Context['env']): Promise<Response | undefined> {
  let required = Boolean(env.TURNSTILE_SECRET_KEY?.trim());
  try {
    if (env.ASSETS) {
      const policy = await env.ASSETS.fetch(new Request(new URL('/rfq-policy.json', request.url)));
      if (!policy.ok) throw Error('policy');
      const value = await policy.json() as { required?: unknown };
      if (typeof value.required !== 'boolean') throw Error('policy');
      required ||= value.required;
    }
    if (required && !env.TURNSTILE_SECRET_KEY?.trim()) throw Error('configuration');
    const ip = request.headers.get('CF-Connecting-IP');
    const cache = (globalThis as unknown as { caches?: { default?: { match(r: Request): Promise<Response | undefined>; put(r: Request, v: Response): Promise<void> } } }).caches?.default;
    if (!ip || !cache) { if (required) throw Error('limiter'); return; }
    const hash = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(ip));
    const key = new Request(new URL('/__rfq_limit/' + Array.from(new Uint8Array(hash), b => b.toString(16).padStart(2, '0')).join(''), request.url));
    const saved = await cache.match(key);
    const now = Date.now();
    let counter = saved ? await saved.json() as { count: number; expires: number } : { count: 0, expires: now + RFQ_WINDOW_SECONDS * 1000 };
    if (counter.expires <= now) counter = { count: 0, expires: now + RFQ_WINDOW_SECONDS * 1000 };
    if (counter.count >= RFQ_IP_LIMIT) return Response.json({ error: 'error.rate' }, { status: 429, headers: { 'Cache-Control': 'no-store', 'Retry-After': String(Math.max(1, Math.ceil((counter.expires - now) / 1000))) } });
    counter.count++;
    await cache.put(key, Response.json(counter, { headers: { 'Cache-Control': `max-age=${Math.max(60, Math.ceil((counter.expires - now) / 1000))}` } }));
  } catch { return reply(503, { error: 'error.storage' }); }
}
const limits = { name: 100, company: 150, email: 254, message: 2000 } as const;
const optionalLimits = { country: 80, product_slug: 120, quantity_note: 200, source_page: 300 } as const;
const reply = (status: number, body: object) => Response.json(body, { status, headers: { 'Cache-Control': 'no-store' } });
export async function onRequestPost({ request, env }: Context): Promise<Response> {
  const blocked = await abuseCheck(request, env);
  if (blocked) return blocked;
  let input: Record<string, unknown>;
  try {
    const raw = await request.text();
    if (raw.length > 12000) return reply(400, { errors: { message: 'error.large' } });
    const body = JSON.parse(raw);
    if (!body || typeof body !== 'object' || Array.isArray(body)) throw Error('shape');
    input = body;
  } catch { return reply(400, { errors: { message: 'error.json' } }); }
  if (typeof input.website === 'string' && input.website.trim()) return new Response(null, { status: 204, headers: { 'Cache-Control': 'no-store' } });
  const errors: Record<string, string> = {};
  const record: Record<string, string | null> = {};
  for (const [field, max] of Object.entries(limits)) {
    const value = input[field];
    if (typeof value !== 'string' || !value.trim() || value.length > max) errors[field] = 'error.required';
    else record[field] = value.trim();
  }
  for (const [field, max] of Object.entries(optionalLimits)) {
    const value = input[field];
    if (value === undefined) continue;
    if (typeof value !== 'string' || value.length > max) errors[field] = 'error.optional';
    else record[field] = value.trim();
  }
  if (record.email && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(record.email)) errors.email = 'error.email';
  if (input.website !== undefined && typeof input.website !== 'string') errors.website = 'error.invalid';
  if (Object.keys(errors).length) return reply(400, { errors });
  if (env.TURNSTILE_SECRET_KEY?.trim()) {
    const token = input['cf-turnstile-response'];
    if (typeof token !== 'string' || !token || token.length > 2048) return reply(403, { error: 'error.challenge' });
    try {
      const verification = await fetch('https://challenges.cloudflare.com/turnstile/v0/siteverify', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, redirect: 'manual', signal: AbortSignal.timeout(5000),
        body: JSON.stringify({ secret: env.TURNSTILE_SECRET_KEY, response: token, remoteip: request.headers.get('CF-Connecting-IP') }),
      });
      if (!verification.ok) throw Error('verification');
      const result = await verification.json() as { success?: boolean; hostname?: string; action?: string };
      if (result.success !== true || result.hostname !== new URL(request.url).hostname || result.action !== 'rfq') throw Error('verification');
    } catch { return reply(403, { error: 'error.challenge' }); }
  }
  record.user_agent = (request.headers.get('User-Agent') || '').slice(0, 300);
  const locale = input.locale === undefined ? new URL(request.url).pathname.split('/')[1] : input.locale;
  record.locale = typeof locale === 'string' && ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar'].includes(locale) ? locale : null;
  try {
    if (!env.SUPABASE_URL || !env.SUPABASE_SECRET_KEY) throw Error('configuration');
    const response = await fetch(`${env.SUPABASE_URL.replace(/\/$/, '')}/rest/v1/apparel_rfq`, {
      method: 'POST',
      headers: { apikey: env.SUPABASE_SECRET_KEY, Authorization: `Bearer ${env.SUPABASE_SECRET_KEY}`, 'Content-Type': 'application/json', Prefer: 'return=representation' },
      body: JSON.stringify(record),
      signal: AbortSignal.timeout(10000),
      // Workers rejects redirect:error; manual also prevents forwarding the secret to redirects.
      redirect: 'manual',
    });
    if (response.status !== 201) throw Error('storage');
    const rows = await response.json();
    if (!Array.isArray(rows) || rows.length !== 1 || typeof rows[0]?.id !== 'string' || !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(rows[0].id)) throw Error('receipt');
    return new Response(null, { status: 204, headers: { 'Cache-Control': 'no-store' } });
  }
  catch { return reply(503, { error: 'error.storage' }); }
}
