/** RFQ contract gate; depends on Bun and Pages handler; CI only, no external records. */
import { test, expect } from 'bun:test';
import { onRequestPost } from '../functions/api/rfq';
const valid = { name: 'CI Buyer', company: 'CI Example', email: 'buyer@example.com', message: 'Please quote a sample.', website: '' };
function fixture(body: unknown, fail = false) {
  const records = new Map<string, string>();
  const request = new Request('https://example.com/api/rfq', { method: 'POST', body: JSON.stringify(body), headers: { 'Content-Type': 'application/json' } });
  return { records, run: () => onRequestPost({ request, env: { RFQ: { put: async (id: string, value: string) => { if (fail) throw Error('unavailable'); records.set(id, value); } } } }) };
}
test('valid RFQ persists one record before returning its id', async () => {
  const f = fixture(valid); const response = await f.run();
  expect(response.status).toBe(201); const { id } = await response.json();
  expect(f.records.size).toBe(1); const record = JSON.parse(f.records.get(id)!); expect(record).toMatchObject({ name: valid.name, email: valid.email, company: valid.company, message: valid.message }); expect(record.website).toBeUndefined();
});
for (const [field, value] of Object.entries({ name: 'n'.repeat(101), company: 'c'.repeat(151), email: 'invalid', message: 'm'.repeat(2001) })) {
  test(`invalid ${field} returns field error without a write`, async () => {
    const f = fixture({ ...valid, [field]: value }); const r = await f.run();
    expect(r.status).toBe(400); expect((await r.json()).errors[field]).toBeTruthy(); expect(f.records.size).toBe(0);
  });
}
for (const body of [null, [], { ...valid, name: 42 }, { ...valid, email: 'a'.repeat(250) + '@x.com' }, { ...valid, message: '' }]) {
  test(`reject malformed input ${JSON.stringify(body).slice(0, 30)}`, async () => { const f = fixture(body); expect((await f.run()).status).toBe(400); expect(f.records.size).toBe(0); });
}
test('honeypot looks successful and stores nothing', async () => { const f = fixture({ ...valid, website: 'bot' }); expect((await f.run()).status).toBe(201); expect(f.records.size).toBe(0); });
test('storage failure does not acknowledge success', async () => { expect((await fixture(valid, true).run()).status).toBe(503); });
