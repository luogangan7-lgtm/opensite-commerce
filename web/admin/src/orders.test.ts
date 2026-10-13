/** Orders owner-contract gates; Bun only, no real backend/auth acceptance. */
import { expect, test } from 'bun:test';
import { formatMoney, parseOrderPage, parseOrderDetail } from './orders';
const id = '11111111-1111-4111-8111-111111111111';
const time = '2026-10-13T00:00:00Z';
const summary = { id, order_number: 'ORD-1', state: 'paid', currency: 'USD', total_minor: '900719925474099301', customer_kind: 'guest', created_at: time };
const detail = { id, order_number: 'ORD-1', state: 'paid', version: 1, currency: 'USD', lines: [{ line_id: id, sku_code: 'SKU-1', title: 'Item', quantity: 2, unit_price_minor: '10', tax_minor: '1', line_total_minor: '20' }], adjustments: [{ id, reason: 'discount', amount_minor: '-3' }], shipping: { amount_minor: '2', method: 'courier' }, tax_total_minor: '1', total_minor: '20', created_at: time, updated_at: time };
test('CONTRACT FIXTURE (orders read API pending PR): exact signed and currency exponent money', () => {
  expect(formatMoney('900719925474099301', 'USD')).toBe('USD 9,007,199,254,740,993.01');
  expect(formatMoney('-900719925474099301', 'USD')).toBe('USD -9,007,199,254,740,993.01');
  expect(formatMoney('123', 'JPY')).toBe('JPY 123');
  expect(formatMoney('123', 'KWD')).toBe('KWD 0.123');
  for (const bad of ['1.2', '1e2', '+2', '01', '-0', '']) expect(() => formatMoney(bad, 'USD')).toThrow();
  expect(() => formatMoney('1', 'zzZ')).toThrow();
});
test('CONTRACT FIXTURE (orders read API pending PR): reject unsafe or malformed summaries', () => {
  expect(parseOrderPage({ items: [summary], next_cursor: 'a+/=' }).items[0].total_minor).toBe(summary.total_minor);
  for (const patch of [{total_minor: 9007199254740992}, {state: 'unknown'}, {id: '../bad'}, {currency: 'zzZ'}, {customer_kind: 'anonymous'}, {created_at: 'today'}, {created_at: '2026-02-30T00:00:00Z'}]) expect(() => parseOrderPage({ items: [{...summary, ...patch}] })).toThrow();
  expect(() => parseOrderPage({items: [], next_cursor: 1})).toThrow();
});
test('CONTRACT FIXTURE (orders read API pending PR): detail nested fields validated', () => {
  expect(parseOrderDetail(detail).adjustments[0].amount_minor).toBe('-3');
  for (const patch of [{version: Number.MAX_SAFE_INTEGER + 1}, {lines: [{...detail.lines[0], quantity: 0}]}, {lines: [{...detail.lines[0], tax_minor: 1}]}, {shipping: {amount_minor: '1'}}, {tax_total_minor: '1e4'}, {adjustments: [{id, amount_minor: '-1'}]}]) expect(() => parseOrderDetail({...detail, ...patch})).toThrow();
});

test('CONTRACT FIXTURE (orders read API pending PR): scoped URLs, pagination and ETag', async () => {
  const {AdminAPI, APIError} = await import('./api');
  const {OrdersAPI} = await import('./orders');
  const calls: string[] = [];
  const responses = [Response.json({items: [summary]}), Response.json(detail, {headers: {'ETag': '"1"'}}), Response.json(detail, {headers: {'ETag': '"2"'}}), Response.json({detail: 'private'}, {status: 404, headers: {'Content-Type': 'application/problem+json'}})];
  const fetcher = (async (path: string | URL | Request) => { calls.push(String(path)); return responses.shift()!; }) as typeof fetch;
  const api = new OrdersAPI(new AdminAPI({fetch: fetcher}));
  await api.list(id, {cursor: 'a+/=', state: 'paid'});
  expect(calls[0]).toBe(`/api/v1/t/${id}/orders?limit=25&cursor=a%2B%2F%3D&state=paid`);
  expect((await api.get(id, id)).version).toBe(1);
  expect(calls[1]).toBe(`/api/v1/t/${id}/orders/${id}`);
  await expect(api.get(id,id)).rejects.toThrow('Invalid order version response');
  await expect(api.get(id,id)).rejects.toBeInstanceOf(APIError);
  for (const limit of [0,101,1.5]) await expect(api.list(id,{limit})).rejects.toThrow();
  await expect(api.list('../auth')).rejects.toThrow();
  await expect(api.get(id,'../private')).rejects.toThrow();
  expect(calls).toHaveLength(4);
});

test('domain currencies preserve original minor units without guessing an exponent', () => {
  expect(parseOrderPage({items:[{...summary,currency:'ZZZ'}]}).items[0].currency).toBe('ZZZ');
  expect(formatMoney('900719925474099301','ZZZ')).toBe('ZZZ 900719925474099301 minor units');
  expect(formatMoney('-123','ABC')).toBe('ABC -123 minor units');
});
