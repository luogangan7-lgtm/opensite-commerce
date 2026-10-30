/** RFQ POST boundary; Pages invokes with private KV binding; never logs or exposes records. */
type Context = { request: Request; env: { RFQ: { put(key: string, value: string): Promise<void> } } };
const limits = { name: 100, company: 150, email: 254, message: 2000 } as const;
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
  if (record.email && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(record.email)) errors.email = 'Enter a valid email address.';
  if (input.website !== undefined && typeof input.website !== 'string') errors.website = 'Invalid field.';
  if (Object.keys(errors).length) return reply(400, { errors });
  const id = crypto.randomUUID();
  try { await env.RFQ.put(id, JSON.stringify({ id, ...record, created_at: new Date().toISOString() })); }
  catch { return reply(503, { error: 'Inquiry not saved. Please try again.' }); }
  return reply(201, { id });
}
