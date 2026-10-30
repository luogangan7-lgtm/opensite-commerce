/** RFQ POST boundary; Pages uses private Supabase bindings; DB owns id/time, no PII logging. */
type Context = { request: Request; env: { SUPABASE_URL: string; SUPABASE_SECRET_KEY: string } };
const limits = { name: 100, company: 150, email: 254, message: 2000 } as const;
const optionalLimits = { country: 80, product_slug: 120, quantity_note: 200, source_page: 300 } as const;
const reply = (status: number, body: object) => Response.json(body, { status, headers: { 'Cache-Control': 'no-store' } });
export async function onRequestPost({ request, env }: Context): Promise<Response> {
  // ponytail: no rate limit in preview; add abuse controls before opening production intake.
  let input: Record<string, unknown>;
  try {
    const raw = await request.text();
    if (raw.length > 12000) return reply(400, { errors: { message: 'Request too large.' } });
    const body = JSON.parse(raw);
    if (!body || typeof body !== 'object' || Array.isArray(body)) throw Error('shape');
    input = body;
  } catch { return reply(400, { errors: { message: 'Send a valid JSON object.' } }); }
  if (typeof input.website === 'string' && input.website.trim()) return reply(201, {});
  const errors: Record<string, string> = {};
  const record: Record<string, string> = {};
  for (const [field, max] of Object.entries(limits)) {
    const value = input[field];
    if (typeof value !== 'string' || !value.trim() || value.length > max) errors[field] = `Required; maximum ${max} characters.`;
    else record[field] = value.trim();
  }
  for (const [field, max] of Object.entries(optionalLimits)) {
    const value = input[field];
    if (value === undefined) continue;
    if (typeof value !== 'string' || value.length > max) errors[field] = `Maximum ${max} characters.`;
    else record[field] = value.trim();
  }
  if (record.email && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(record.email)) errors.email = 'Enter a valid email address.';
  if (input.website !== undefined && typeof input.website !== 'string') errors.website = 'Invalid field.';
  if (Object.keys(errors).length) return reply(400, { errors });
  record.user_agent = (request.headers.get('User-Agent') || '').slice(0, 300);
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
    return reply(201, { id: rows[0].id });
  }
  catch { return reply(503, { error: 'Inquiry not saved. Please try again.' }); }
}
