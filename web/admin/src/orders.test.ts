/** Orders owner-contract gates; Bun only, no real backend/auth acceptance. */
import { expect, test } from 'bun:test';
import { formatMoney, parseOrderPage, parseOrderDetail } from './orders';
const id = '11111111-1111-4111-8111-111111111111';
const time = '2026-10-13T00:00:00Z';
const summary = { id, order_number: 1001, state: 'paid', currency: 'USD', total_minor: '900719925474099301', customer_kind: 'guest', created_at: time };
const detail = { id, order_number: 1001, state: 'paid', version: 1, currency: 'USD', lines: [{ line_id: id, sku_code: 'SKU-1', title: null, quantity: 2, unit_price_minor: '10', tax_minor: '1', line_total_minor: '20' }], adjustments: [{ id, reason: 'discount', amount_minor: '-3' }], shipping: { amount_minor: '2', method: 'by_weight' }, tax_total_minor: '1', total_minor: '20', created_at: time, updated_at: time };
test('PR47 projection accepts null snapshot titles and nullable shipping rules', () => {
  for (const method of [null, 'fixed', 'by_weight', 'free_over_amount', '', 'future_rule']) {
    const parsed = parseOrderDetail({...detail, shipping: {...detail.shipping, method}});
    expect(parsed.lines[0].title).toBeNull();
    expect(parsed.lines[0].sku_code).toBe('SKU-1');
    expect(parsed.shipping.method).toBe(method);
  }
  // The published schema also permits a title string; current snapshots return null.
  expect(parseOrderDetail({...detail, lines: [{...detail.lines[0], title: 'Future title'}]}).lines[0].title).toBe('Future title');
  for (const value of [undefined, 1, {}]) {
    expect(() => parseOrderDetail({...detail, lines: [{...detail.lines[0], title: value}]})).toThrow();
    expect(() => parseOrderDetail({...detail, shipping: {...detail.shipping, method: value}})).toThrow();
  }
});
test('PR47 final page omits cursor and adjustments depend only on signed amounts', () => {
  expect(parseOrderPage({items: []}).next_cursor).toBeUndefined();
  expect(() => parseOrderPage({items: [], next_cursor: null})).toThrow();
  const parsed = parseOrderDetail({...detail, guest_email: 'private@example.invalid', adjustments: [
    {id, reason: 'discount', amount_minor: '-3'}, {id, reason: 'surcharge', amount_minor: '3'},
  ]});
  expect(parsed.adjustments.map(a => formatMoney(a.amount_minor, 'USD'))).toEqual(['USD -0.03', 'USD 0.03']);
  expect(parsed).not.toHaveProperty('guest_email');
  expect(parsed.adjustments[0]).not.toHaveProperty('kind');
});
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

test('PR47 order numbers are positive JSON integers, not fabricated string identifiers', () => {
  expect(parseOrderPage({items: [summary]}).items[0].order_number).toBe(1001);
  expect(parseOrderDetail(detail).order_number).toBe(1001);
  for (const order_number of ['ORD-1001', '1001', 0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1]) {
    expect(() => parseOrderPage({items: [{...summary, order_number}]})).toThrow();
    expect(() => parseOrderDetail({...detail, order_number})).toThrow();
  }
});

test('CONTRACT FIXTURE (orders read API pending PR): uppercase ids are accepted and sent lowercase', async () => {
  const {AdminAPI} = await import('./api');
  const {OrdersAPI} = await import('./orders');
  const calls: string[] = [];
  const api = new OrdersAPI(new AdminAPI({fetch: (async (path: string | URL | Request) => { calls.push(String(path)); return Response.json({items: []}); }) as typeof fetch}));
  const upper = 'AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA';
  await api.list(upper);
  expect(calls[0]).toBe(`/api/v1/t/${upper.toLowerCase()}/orders?limit=25`);
});

// The UI represents the first page as an empty cursor; the API rejects cursor=.
test('first orders page omits an empty cursor from the generated query', async () => {
  const {AdminAPI} = await import('./api');
  const {OrdersAPI} = await import('./orders');
  const calls: string[] = [];
  const api = new OrdersAPI(new AdminAPI({fetch: (async (path: string | URL | Request) => {
    calls.push(String(path)); return Response.json({items: []});
  }) as typeof fetch}));
  await api.list(id, {cursor: ''});
  expect(calls).toEqual([`/api/v1/t/${id}/orders?limit=25`]);
});
