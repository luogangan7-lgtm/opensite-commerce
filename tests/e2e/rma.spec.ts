/** Real purchase → shipments → RMA → Payments refund, in both viewport projects.
 * Dependencies: shared purchase UI, real staff session/API and Returns coordinator.
 * No fulfillment write UI exists; use its public API and render the real order UI.
 * SQL is reserved for run.py's independent persisted witnesses, never business setup.
 */
import { test, expect, type APIResponse } from '@playwright/test';
import { fixture, publishedSite, audit, fillPassword } from './fixture';
import { start } from './purchase-helpers';

// Credentials/capabilities must never enter automatic capture artifacts.
test.use({ trace: 'off', video: 'off', screenshot: 'off' });

test('real shipment → partial → full refund journey', async ({ page, request }, info) => {
  test.setTimeout(180_000);
  await publishedSite(request);
  const checkout = await start(page, false, 2);
  await page.getByRole('button', { name: 'Fake payment: capture', exact: true }).click();
  await page.getByRole('link', { name: 'View order confirmation', exact: true }).click();
  await expect(page.locator('[data-completion-ready]')).toBeVisible();
  expect(await (await page.request.get(`${fixture.storefront}/purchase/api/completion`)).json())
    .toMatchObject({ order_id: checkout.order_id, state: 'paid', payment: { status: 'captured' } });

  const base = `${fixture.admin}/api/v1/t/${fixture.tenantId}`;
  const orderPath = `/orders/${checkout.order_id}`;
  await page.goto(`${fixture.admin}/t/${fixture.tenantId}${orderPath}`);
  await page.getByLabel('Email', { exact: true }).fill('rma-staff@example.test');
  await fillPassword(page, fixture.ownerPassword);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('cell', { name: 'C24-CANVAS-TOTE', exact: true })).toBeVisible();
  const session = await (await page.request.get(`${fixture.admin}/api/v1/auth/session`)).json();
  const read = async () => {
    const response = await page.request.get(base + orderPath);
    expect(response.status()).toBe(200);
    return response.json();
  };
  const write = (path: string, data: unknown, version: number, key = crypto.randomUUID(), tenant: string = fixture.tenantId) =>
    page.request.post(`${fixture.admin}/api/v1/t/${tenant}${path}`, { data, headers: {
      Origin: fixture.admin, 'X-CSRF-Token': session.csrf_token,
      'Idempotency-Key': key, 'If-Match': `"${version}"`,
    } });
  const accepted = async (op: string, response: APIResponse, status: number) => {
    let body;
    try { body = await response.json(); }
    catch { throw new Error(`${op}: status=${response.status()} code=invalid_json`); }
    const code = body.code === 'route_not_found' ? 'route_not_found' : response.ok() ? 'none' : 'unknown';
    const label = `${op}: status=${response.status()} code=${code}`;
    if (response.status() === 503 || body.code === 'route_not_found' ||
        (op === 'refundReturn' && response.status() !== status))
      throw new Error(label);
    expect(response.status(), label).toBe(status);
    return body;
  };
  let order = await read();
  expect(order).toMatchObject({ state: 'paid', total_minor: '5780', lines: [{ quantity: 2 }] });
  const line = order.lines[0].line_id;
  const shipment = (n: number) => ({ carrier: 'E2E-RMA', tracking_number: `rma-${info.project.name}-${n}`,
    lines: [{ order_line_id: line, quantity: 1 }] });
  const first = await accepted('createShipment', await write(orderPath + '/shipments', shipment(1), order.version), 201);
  order = await read();
  expect(order.state).toBe('partially_fulfilled');
  expect(order.version).toBe(first.order_version);
  const second = await accepted('createShipment', await write(orderPath + '/shipments', shipment(2), order.version), 201);
  order = await read();
  expect(order.state).toBe('fulfilled');
  expect(order.version).toBe(second.order_version);
  expect(second.id).not.toBe(first.id);

  const requestReturn = { kind: 'shipment_return', reason: 'E2E-RMA', lines: [
    { order_line_id: line, shipment_line_id: first.lines[0].id, quantity: 1 },
  ] };
  const rma = await accepted('createReturn', await write(orderPath + '/returns', requestReturn, order.version), 201);
  expect(rma).toMatchObject({ status: 'requested', restocked: false, refund_minor: '2890' });
  const returnPath = `/returns/${rma.id}`;
  const receiptKey = crypto.randomUUID();
  const received = await accepted('receiveReturn', await write(returnPath + '/receive', { restock: true }, rma.order_version, receiptKey), 200);
  expect(received).toMatchObject({ id: rma.id, status: 'received', restocked: true });
  const replay = await accepted('receiveReturn', await write(returnPath + '/receive', { restock: true }, rma.order_version, receiptKey), 200);
  expect(replay).toEqual(received);
  // A fresh command identity cannot receive/restock the same RMA again.
  const duplicate = await write(returnPath + '/receive', { restock: true }, received.order_version);
  expect([409, 422]).toContain(duplicate.status());
  const foreign = await write(returnPath + '/receive', { restock: true }, received.order_version,
    crypto.randomUUID(), fixture.ownerTenantId);
  expect(foreign.status()).toBe(404);
  expect((await page.request.get(`${fixture.admin}/api/v1/t/${fixture.ownerTenantId}${orderPath}`)).status()).toBe(404);
  // Money is server owned: injected refund amount is rejected before any PSP action.
  expect((await write(returnPath + '/refund', { amount_minor: '5781' }, received.order_version)).status()).toBe(400);
  const refundKey = crypto.randomUUID();
  const refunded = await accepted('refundReturn', await write(returnPath + '/refund', {}, received.order_version, refundKey), 200);
  expect(refunded).toMatchObject({ id: rma.id, status: 'refunded', restocked: true, refund_minor: '2890' });
  expect(await accepted('refundReturn', await write(returnPath + '/refund', {}, received.order_version, refundKey), 200)).toEqual(refunded);
  order = await read();
  expect(order.state).toBe('partially_refunded');
  // Returning an already returned shipment unit would exceed its original entitlement.
  const excess = await write(orderPath + '/returns', requestReturn, order.version);
  expect([409, 422]).toContain(excess.status());
  expect(await read()).toEqual(order);
  // A distinct shipped unit needs its own RMA and Payments command identity.
  const remaining = await accepted('createReturn', await write(orderPath + '/returns', {
    ...requestReturn, lines: [{ order_line_id: line, shipment_line_id: second.lines[0].id, quantity: 1 }],
  }, order.version), 201);
  expect(remaining).toMatchObject({ status: 'requested', restocked: false, refund_minor: '2890' });
  expect(remaining.id).not.toBe(rma.id);
  const remainingPath = `/returns/${remaining.id}`;
  const remainingReceived = await accepted('receiveReturn', await write(remainingPath + '/receive',
    { restock: true }, remaining.order_version), 200);
  expect(remainingReceived).toMatchObject({ id: remaining.id, status: 'received', restocked: true });
  const fullRefundKey = crypto.randomUUID();
  expect(fullRefundKey).not.toBe(refundKey);
  const fullRefund = await accepted('refundReturn', await write(remainingPath + '/refund', {},
    remainingReceived.order_version, fullRefundKey), 200);
  expect(fullRefund).toMatchObject({ id: remaining.id, status: 'refunded', restocked: true, refund_minor: '2890' });
  expect(await accepted('refundReturn', await write(remainingPath + '/refund', {},
    remainingReceived.order_version, fullRefundKey), 200)).toEqual(fullRefund);
  order = await read();
  expect(order).toMatchObject({ state: 'refunded', total_minor: '5780' });
  expect(BigInt(refunded.refund_minor) + BigInt(fullRefund.refund_minor)).toBe(BigInt(order.total_minor));
  expect(await read()).toEqual(order);
  await page.reload();
  await expect(page.getByRole('main')).toContainText('refunded');
  await audit(page, info);
  await info.attach('rma-journey', { body: JSON.stringify({ first, second, received, refunded, remainingReceived, fullRefund, order }), contentType: 'application/json' });
});
