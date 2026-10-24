/** Responsibility: real CMS publication -> browser purchase -> signed Payments -> persisted Checkout.
 * Dependencies: production Astro BFF, opt-in bootstrap composition, CI-only owned seed.
 * Invariants: no interception/fabricated receipt; pending receipt alone never means paid.
 */
import { createHmac } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { audit, fixture, publishedSite } from './fixture';
import { start, registerAndLogin } from './purchase-helpers';

// Credentials/capabilities must never enter automatic capture artifacts.
test.use({ trace: 'off', video: 'off', screenshot: 'off' });

const api = `${fixture.storefront}/purchase/api`;
test('published guest purchase persists captured order', async ({ page, request }, info) => {
  const site = await publishedSite(request);
  expect(site.release_id).not.toBe('c0900005-0000-4000-8000-000000000001');
  const checkout = await start(page);
  const csrf=(await (await page.request.get(`${api}/context`)).json()).csrf;
  for (const accepted_total of [undefined,{amount_minor:'3139',currency:'USD'},{amount_minor:'3140',currency:'EUR'}]) {
    const response=await page.request.post(`${api}/payment`,{headers:{Origin:fixture.storefront,'X-CSRF-Token':csrf},data:{checkout_id:checkout.checkout_id,outcome:'captured',accepted_total}});
    expect(response.status()).toBe(409);expect(await response.json()).toMatchObject({code:'checkout_total_changed'});
    expect(await (await page.request.get(`${api}/completion`)).json()).toMatchObject({state:'ready',payment:{status:'authorized'}});
  }
  await audit(page, info);
  const paid = page.waitForResponse(r => r.url() === `${api}/payment` && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Fake payment: capture', exact: true }).click();
  expect((await paid).status()).toBe(200);
  expect(await (await paid).json()).toMatchObject({ checkout_id: checkout.checkout_id,
    order_id: checkout.order_id, state: 'paid', payment: { status: 'captured' } });
  await page.getByRole('link', { name: 'View order confirmation', exact: true }).click();
  await expect(page.locator('[data-completion-ready]')).toBeVisible();
  await expect(page.getByRole('main')).toContainText('$31.40');
  // Fresh read from the BFF traverses the real authenticated Get path again.
  const persisted = await page.request.get(`${api}/completion`);
  expect(persisted.status()).toBe(200);
  expect(await persisted.json()).toMatchObject({ checkout_id: checkout.checkout_id,
    order_id: checkout.order_id, state: 'paid', payment: { status: 'captured' } });
  const replay=await page.request.post(`${api}/payment`,{headers:{Origin:fixture.storefront,'X-CSRF-Token':csrf},data:{checkout_id:checkout.checkout_id,outcome:'captured',accepted_total:{amount_minor:'3140',currency:'USD'}}});
  expect(replay.status()).toBe(200);expect(await replay.json()).toMatchObject({state:'paid',payment:{status:'captured'}});
  await info.attach('purchase-checkout', { body: JSON.stringify(await persisted.json()), contentType: 'application/json' });
  await audit(page, info);
});

test('pending receipt alone never shows paid', async ({ page, request }, info) => {
  await publishedSite(request);
  const checkout = await start(page);
  const key = process.env.OPENSITE_FAKE_WEBHOOK_KEY;
  if (!key) throw new Error('Missing isolated CI signing key');
  const event = { event_id: `c09d:${info.project.name}:refund-before-capture`,
    refund_id: `c09d:${info.project.name}:refund`, account_id: 'c09d-e2e',
    order_id: checkout.order_id, order_version: Number(checkout.order_version),
    provider_ref: checkout.payment.provider_ref, status: 'refunded', amount_minor: 3140, currency: 'USD' };
  const raw = JSON.stringify(event), timestamp = String(Math.floor(Date.now() / 1000));
  const signature = `t=${timestamp},v1=${createHmac('sha256', Buffer.from(key, 'base64url')).update(`${timestamp}.${raw}`).digest('hex')}`;
  const receipt = await request.post(`${fixture.api}/api/v1/testing/payments/fake/webhook`, {
    data: raw, headers: { 'Content-Type': 'application/json', 'X-Fake-Signature': signature }, maxRedirects: 0,
  });
  expect(receipt.status()).toBe(200);
  expect(await receipt.json()).toEqual({ event_id: event.event_id, status: 'pending' });
  await page.goto(`${fixture.storefront}/order-complete`);
  await expect(page.locator('[data-completion-page]')).toHaveAttribute('aria-busy', 'false');
  await expect(page.locator('[data-completion-ready]')).toHaveCount(0);
  const persisted = await page.request.get(`${api}/completion`);
  expect(persisted.status()).toBe(200);
  expect(await persisted.json()).toMatchObject({ checkout_id: checkout.checkout_id,
    state: 'ready', payment: { status: 'authorized' } });
  await audit(page, info);
});


test('registered purchase appears only in owned history',async({page,request},info)=>{
  test.setTimeout(90_000);
  await publishedSite(request);
  await registerAndLogin(page,`c09f2-${info.project.name}@example.test`);
  const checkout=await start(page,true);
  await page.getByRole('button',{name:'Fake payment: capture',exact:true}).click();
  await page.getByRole('link',{name:'View order confirmation',exact:true}).click();
  await expect(page.locator('[data-completion-ready]')).toBeVisible();
  const backend=`${fixture.api}/api/v1/t/${fixture.tenantId}/customer/orders`;
  const list=await page.request.get(backend);expect(list.status()).toBe(200);
  const history=await list.json();expect(history.items).toHaveLength(1);expect(history.items[0]).toMatchObject({id:checkout.order_id,state:'paid',total_minor:'3140'});
  await page.goto(`${fixture.storefront}/account/orders`);
  await expect(page.getByRole('button',{name:`Order #${history.items[0].order_number}`,exact:true})).toBeVisible();
  await audit(page,info);
  await page.getByRole('button',{name:`Order #${history.items[0].order_number}`,exact:true}).click();
  await expect(page.getByRole('region',{name:'Order details'})).toContainText('C24-CANVAS-TOTE');
  await audit(page,info);
  const oldSession=(await page.context().cookies()).find(c=>c.name==='__Host-osc_customer_session');
  expect(oldSession).toBeDefined();
  await page.goto(`${fixture.storefront}/account`);await page.getByRole('button',{name:'Sign out',exact:true}).click();
  await expect(page.getByRole('button',{name:'Sign in',exact:true})).toBeVisible();
  expect((await page.request.get(backend)).status()).toBe(401);
  await page.goto(`${fixture.storefront}/account/orders`);await expect(page.getByRole('alert')).toHaveText('Sign in to view your orders.');
  // A revoked, formerly valid session must not fall through to the guest checkout.
  await page.context().addCookies([oldSession!]);
  expect((await page.request.get(`${api}/history`)).status()).toBe(401);
  // Invalid-session reads clear the cookie; replay it on each protected request.
  await page.context().addCookies([oldSession!]);
  expect((await page.request.get(`${api}/completion`)).status()).toBe(401);
  await registerAndLogin(page,`c09f2-other-${info.project.name}@example.test`);
  expect(await (await page.request.get(backend)).json()).toMatchObject({items:[]});
  expect((await page.request.get(`${backend}/${checkout.order_id}`)).status()).toBe(404);
  expect((await page.request.get(`${api}/history/${checkout.order_id}`)).status()).toBe(404);
  await page.context().clearCookies();
  await page.context().addCookies([{...oldSession!,value:'A'.repeat(43)}]);
  expect((await page.request.get(backend)).status()).toBe(401);
  expect((await page.request.get(`${api}/history`)).status()).toBe(401);
  await page.context().clearCookies();
  await page.goto(`${fixture.admin}/login`);await page.getByLabel('Email',{exact:true}).fill(fixture.ownerEmail);await page.getByLabel('Password',{exact:true}).fill(fixture.ownerPassword);
  await page.getByRole('button',{name:'Sign in',exact:true}).click();
  await expect(page.getByRole('heading',{name:'Open a store',exact:true})).toBeVisible();
  expect((await page.request.get(backend)).status()).toBe(401);
});
