/** Responsibility: real B2B inquiry -> negotiated quote -> company approval -> paid order.
 * Dependencies: production API (separate opt-in composition), real account/admin UI,
 * CI identity-only seed and signed Fake provider used by purchase.spec.ts.
 * Invariants: API business writes only; no SQL/interception/fabricated receipts.
 * Quote/inquiry/approval UI does not exist: those steps use their real HTTP APIs.
 */
import { createHmac, randomUUID } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { audit, fixture, publishedSite } from './fixture';

const api = 'http://127.0.0.1:8082';
const tenant = `${api}/api/v1/t/${fixture.tenantId}`;
const publicAPI = `${api}/api/public/v1`;
const storeOrigin = 'https://store.e2e.test';
async function login(page: Page, email: string) {
  await page.goto(`${fixture.storefront}/account`);
  await expect(page.locator('[data-customer-ready]')).toBeVisible();
  await page.getByLabel('Email', { exact: true }).fill(email);
  await page.getByLabel('Password', { exact: true }).fill(fixture.ownerPassword);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('link', { name: 'Order history', exact: true })).toBeVisible();
  const response = await page.request.get(`${tenant}/customer/session`, { headers: { Origin: storeOrigin } });
  expect(response.status()).toBe(200);
  return { Host: '127.0.0.1', Origin: storeOrigin, 'X-CSRF-Token': (await response.json()).csrf_token };
}

test('real B2B inquiry quote approval prepayment order', async ({ page, browser, request }, info) => {
  test.setTimeout(180_000);
  await publishedSite(request);
  const staff = await browser.newPage({ viewport: page.viewportSize()! });
  const approver = await browser.newPage({ viewport: page.viewportSize()! });
  const purchaser = await browser.newPage({ viewport: page.viewportSize()! });
  const outsider = await browser.newPage({ viewport: page.viewportSize()! });
  try {
    const email = `b2b-${info.project.name}-buyer@example.test`;
    const buyerHeaders = await login(page, email);
    await audit(page, info);
    // Host-derived public site; body contains no tenant/company/actor authority.
    const inquiryResponse = await page.request.post(`${publicAPI}/inquiries`, { headers: { Host: '127.0.0.1' }, data: {
      buyer_email: email, buyer_name: 'B2B buyer', message: `B2B ${info.project.name}`, quantity: 2,
      product_id: 'c0900007-0000-4000-8000-000000000002',
    } });
    expect(inquiryResponse.status()).toBe(202);
    const inquiry = await inquiryResponse.json();
    expect(inquiry.status).toBe('received');
    await staff.goto(`${fixture.admin}/login`);
    await staff.getByLabel('Email', { exact: true }).fill('b2b-staff@example.test');
    await staff.getByLabel('Password', { exact: true }).fill(fixture.ownerPassword);
    await staff.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(staff.getByRole('heading', { name: 'Open a store', exact: true })).toBeVisible();
    const staffSession = await (await staff.request.get(`${fixture.admin}/api/v1/auth/session`)).json();
    const staffHeaders = { Host: '127.0.0.1', Origin: 'https://admin.e2e.test', 'X-CSRF-Token': staffSession.csrf_token };
    const quoteResponse = await staff.request.post(`${tenant}/quotes`, { headers: {
      ...staffHeaders, 'If-Match': '*', 'Idempotency-Key': randomUUID(),
    }, data: {
      site_id: fixture.siteId, inquiry_id: inquiry.id,
      company_id: `b2b00002-0000-4000-8000-${info.project.name === 'desktop' ? '000000000001' : '000000000003'}`,
      currency: 'USD', expires_at: '2030-01-01T00:00:00Z',
      lines: [{ product_id: 'c0900007-0000-4000-8000-000000000002', variant_id: 'c2400008-0000-4000-8000-000000000002',
        description: 'Negotiated ceramic mugs', quantity: 2, unit_price: 1000, tax_category: 'standard' }],
      destination: { country: 'US' }, shipping_profile_id: 'c2400014-0000-4000-8000-000000000001', weight_g: 200,
    } });
    expect(quoteResponse.status(), await quoteResponse.text()).toBe(201);
    const quote = await quoteResponse.json();
    expect(quote).toMatchObject({ status: 'draft', snapshot: { subtotal: 2000, tax_amount: 200, shipping_amount: 500, total: 2700 } });
    const sellerURL = `${tenant}/quotes/${quote.id}`, buyerURL = `${publicAPI}/quotes/${quote.id}`;
    const transition = (actor: Page, url: string, action: string, headers: Record<string, string>, etag: string) =>
      actor.request.post(`${url}/${action}`, { data: {}, headers: { ...headers, 'If-Match': etag, 'Idempotency-Key': randomUUID() } });
    const sent = await transition(staff, sellerURL, 'send', staffHeaders, quoteResponse.headers().etag);
    expect(sent.status()).toBe(200);
    expect((await sent.json()).status).toBe('sent');
    const foreignHeaders = await login(outsider, `b2b-${info.project.name}-outsider@example.test`);
    expect((await outsider.request.get(buyerURL, { headers: foreignHeaders })).status()).toBe(404);
    const convertURL = `${sellerURL}/orders`, key = randomUUID();
    const convert = () => page.request.post(convertURL, { data: {}, headers: { ...buyerHeaders, 'Idempotency-Key': key } });
    expect((await outsider.request.post(convertURL, { data: {}, headers: { ...foreignHeaders, 'Idempotency-Key': randomUUID() } })).status()).toBe(404);
    // Real membership admission succeeds; the business rule must deny conversion.
    expect((await convert()).status()).toBe(409);
    const accepted = await transition(page, buyerURL, 'accept', buyerHeaders, sent.headers().etag);
    expect(accepted.status()).toBe(200);
    expect((await accepted.json()).status).toBe('accepted_pending_approval');
    expect((await convert()).status()).toBe(409);
    expect((await transition(page, buyerURL, 'approve', buyerHeaders, accepted.headers().etag)).status()).toBe(403);
    expect((await transition(staff, buyerURL, 'approve', staffHeaders, accepted.headers().etag)).status()).toBe(401);
    const purchaserHeaders = await login(purchaser, `b2b-${info.project.name}-purchaser@example.test`);
    expect((await transition(purchaser, buyerURL, 'approve', purchaserHeaders, accepted.headers().etag)).status()).toBe(403);
    const approverHeaders = await login(approver, `b2b-${info.project.name}-approver@example.test`);
    const approved = await transition(approver, buyerURL, 'approve', approverHeaders, accepted.headers().etag);
    expect(approved.status()).toBe(200);
    const frozen = await approved.json();
    expect(frozen.status).toBe('approved');
    expect(frozen.snapshot).toEqual(quote.snapshot);
    // No edit API exists. Re-sending/withdrawing an approved quote must not mutate it.
    expect((await transition(staff, sellerURL, 'withdraw', staffHeaders, approved.headers().etag)).status()).toBe(409);
    expect((await transition(staff, sellerURL, 'send', staffHeaders, approved.headers().etag)).status()).toBe(409);
    const conversion = await convert();
    expect(conversion.status(), await conversion.text()).toBe(201);
    const receipt = await conversion.json();
    expect(receipt).toMatchObject({ quote_id: quote.id, amount: '2700', currency: 'USD', state: 'pending_payment' });
    expect(receipt.provider_ref).toMatch(/^fake_/);
    const orderURL = `${tenant}/customer/orders/${receipt.order_id}`;
    const orderBefore = await page.request.get(orderURL, { headers: buyerHeaders });
    expect(orderBefore.status()).toBe(200);
    const order = await orderBefore.json();
    expect(order).toMatchObject({ order: { state: 'pending_payment', total_minor: '2700', currency: 'USD' } });
    const payable = await staff.request.get(`${tenant}/orders/${receipt.order_id}`, { headers: staffHeaders });
    expect(payable.status()).toBe(200);
    const payableOrder = await payable.json();
    // Same timestamped, signed mapped provider ingress as the B2C Fake payment.
    const secret = process.env.OPENSITE_FAKE_WEBHOOK_KEY;
    if (!secret) throw new Error('Missing isolated Fake signing key');
    const event = { event_id: `b2b:${info.project.name}:captured`, account_id: 'c09d-e2e',
      order_id: receipt.order_id, order_version: Number(payableOrder.version), provider_ref: receipt.provider_ref,
      status: 'captured', amount_minor: 2700, currency: 'USD' };
    const raw = JSON.stringify(event), timestamp = String(Math.floor(Date.now() / 1000));
    const signature = `t=${timestamp},v1=${createHmac('sha256', Buffer.from(secret, 'base64url')).update(`${timestamp}.${raw}`).digest('hex')}`;
    for (let attempt = 0; attempt < 2; attempt++) {
      const payment = await request.post(`${api}/api/v1/testing/payments/fake/webhook`, { data: raw,
        headers: { 'Content-Type': 'application/json', 'X-Fake-Signature': signature } });
      expect(payment.status(), await payment.text()).toBe(200);
      expect(await payment.json()).toEqual({ event_id: event.event_id, status: 'processed' });
    }
    const persisted = await page.request.get(orderURL, { headers: buyerHeaders });
    expect(persisted.status()).toBe(200);
    expect(await persisted.json()).toMatchObject({ order: { id: receipt.order_id, state: 'paid', total_minor: '2700', currency: 'USD' } });
    expect((await outsider.request.get(orderURL, { headers: foreignHeaders })).status()).toBe(404);
    const replay = await convert();
    expect(replay.status()).toBe(201);
    expect(await replay.json()).toMatchObject({ order_id: receipt.order_id, state: 'paid', amount: '2700' });
    const sellerRead = await staff.request.get(`${sellerURL}/order-conversion`, { headers: staffHeaders });
    expect(sellerRead.status()).toBe(200);
    expect(await sellerRead.json()).toMatchObject({ order_id: receipt.order_id, state: 'paid', amount: '2700' });
    const reread = await page.request.get(buyerURL, { headers: buyerHeaders });
    expect(reread.status()).toBe(200);
    expect(await reread.json()).toEqual(frozen);
    expect(reread.headers().etag).toBe(approved.headers().etag);
    await page.goto(`${fixture.storefront}/account/orders`);
    await page.getByRole('button', { name: `Order #${receipt.order_number}`, exact: true }).click();
    await expect(page.getByRole('region', { name: 'Order details' })).toContainText('$27.00');
    await audit(page, info);
    await info.attach('b2b-paid-order', { body: JSON.stringify(await persisted.json()), contentType: 'application/json' });
  } finally {
    await Promise.all([staff.close(), approver.close(), outsider.close(), purchaser.close()]);
  }
});
