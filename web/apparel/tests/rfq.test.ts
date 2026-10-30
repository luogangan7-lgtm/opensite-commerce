/** RFQ contract gate; Bun stubs PostgREST, never contacts external storage or records real PII. */
import { afterEach, test, expect, mock, spyOn } from 'bun:test';
import { onRequestPost } from '../functions/api/rfq';
const id = '524f5000-0000-4000-8000-000000000001';
const env = { SUPABASE_URL: 'https://database.example.invalid', SUPABASE_SECRET_KEY: 'ci-private-placeholder' };
const valid = { name: 'CI Buyer', company: 'CI Example', email: 'buyer@example.invalid', message: 'Please quote a sample.', website: '' };
afterEach(() => mock.restore());
function fixture(body: unknown, upstream: Response | Error = Response.json([{ id }], { status: 201 })) {
  const writes: Request[] = [];
  spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
    writes.push(new Request(input, init));
    if (upstream instanceof Error) throw upstream;
    return upstream;
  });
  const request = new Request('https://example.invalid/api/rfq', { method: 'POST', body: JSON.stringify(body), headers: { 'Content-Type': 'application/json', 'User-Agent': 'CI RFQ gate' } });
  return { writes, run: (bindings = env) => onRequestPost({ request, env: bindings }) };
}
test('valid RFQ inserts validated fields through private PostgREST and returns the DB id', async () => {
  const f = fixture({ ...valid, country: 'Test', product_slug: 'crew-fleece-8900-32', quantity_note: 'Sample request', source_page: '/contact', id: 'untrusted', created_at: 'untrusted', unknown: 'discard' });
  const response = await f.run();
  expect(response.status).toBe(201); expect(await response.json()).toEqual({ id });
  expect(f.writes).toHaveLength(1);
  const write = f.writes[0];
  expect(write.url).toBe(`${env.SUPABASE_URL}/rest/v1/apparel_rfq`);
  expect(write.method).toBe('POST');
  expect(write.headers.get('apikey')).toBe(env.SUPABASE_SECRET_KEY);
  expect(write.headers.get('Authorization')).toBe(`Bearer ${env.SUPABASE_SECRET_KEY}`);
  expect(write.headers.get('Prefer')).toBe('return=representation');
  expect(write.headers.get('Content-Type')).toBe('application/json');
  expect(await write.json()).toEqual({ name: valid.name, company: valid.company, email: valid.email, message: valid.message, country: 'Test', product_slug: 'crew-fleece-8900-32', quantity_note: 'Sample request', source_page: '/contact', user_agent: 'CI RFQ gate' });
  expect(response.headers.get('Cache-Control')).toBe('no-store');
});
for (const [field, value] of Object.entries({ name: 'n'.repeat(101), company: 'c'.repeat(151), email: 'invalid', message: 'm'.repeat(2001), country: 'c'.repeat(81), product_slug: 'p'.repeat(121), quantity_note: 'q'.repeat(201), source_page: 's'.repeat(301) })) {
  test(`invalid ${field} returns field error without a write`, async () => {
    const f = fixture({ ...valid, [field]: value }); const r = await f.run();
    expect(r.status).toBe(400); expect((await r.json()).errors[field]).toBeTruthy(); expect(f.writes).toHaveLength(0);
  });
}
for (const body of [null, [], { ...valid, name: 42 }, { ...valid, email: 'a'.repeat(250) + '@x.com' }, { ...valid, message: '' }, { ...valid, country: {} }, { ...valid, website: 42 }]) {
  test(`reject malformed input ${JSON.stringify(body).slice(0, 30)}`, async () => { const f = fixture(body); expect((await f.run()).status).toBe(400); expect(f.writes).toHaveLength(0); });
}
test('honeypot looks successful and makes no PostgREST call', async () => { const f = fixture({ ...valid, website: 'bot' }); expect((await f.run()).status).toBe(201); expect(f.writes).toHaveLength(0); });
for (const upstream of [new Error('network'), Response.json({ error: 'private upstream details' }, { status: 403 }), Response.json([], { status: 201 }), Response.json([{ id: 'bad-id' }], { status: 201 }), new Response('not JSON', { status: 201 })]) {
  test(`storage failure or missing DB receipt does not acknowledge success (${upstream instanceof Error ? 'network' : upstream.status})`, async () => {
    const response = await fixture(valid, upstream).run(); expect(response.status).toBe(503);
    expect(await response.json()).toEqual({ error: 'Inquiry not saved. Please try again.' });
  });
}
test('missing private configuration makes no request', async () => {
  const f = fixture(valid); expect((await f.run({ ...env, SUPABASE_SECRET_KEY: '' })).status).toBe(503); expect(f.writes).toHaveLength(0);
});
