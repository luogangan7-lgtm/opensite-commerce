/** Responsibility: real CMS publication -> browser purchase -> signed Payments -> persisted Checkout.
 * Dependencies: production Astro BFF, opt-in bootstrap composition, CI-only owned seed.
 * Invariants: no interception/fabricated receipt; pending receipt alone never means paid.
 */
import { createHmac } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { audit, fixture, publishedSite } from './fixture';

const api = `${fixture.storefront}/purchase/api`;
async function start(page: Page, customer = false) {
  await page.goto(`${fixture.storefront}/products/${fixture.productSlug}`);
  await expect(page.getByRole('button', { name: 'Add to cart', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Add to cart', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('Item added to cart.');
  const cookie = (await page.context().cookies()).find(c => c.name === '__Host-osc_storefront_cart');
  expect(cookie && { secure: cookie.secure, httpOnly: cookie.httpOnly, sameSite: cookie.sameSite, path: cookie.path })
    .toEqual({ secure: true, httpOnly: true, sameSite: 'Lax', path: '/' });
  expect(await page.evaluate(() => document.cookie)).not.toContain('__Host-osc_storefront_cart');
  await page.goto(`${fixture.storefront}/cart`);
  await expect(page.locator('[data-cart-line]')).toHaveCount(1);
  await expect(page.getByRole('main')).toContainText('$24.00');
  await page.goto(`${fixture.storefront}/checkout`);
  if (!customer) await page.getByLabel('Email', { exact: true }).fill('c09d-buyer@example.test');
  await page.getByLabel('Country code', { exact: true }).fill('US');
  await page.getByLabel('Address line 1', { exact: true }).fill('1 Test Way');
  await page.getByLabel('City', { exact: true }).fill('Test City');
  await page.getByLabel('Postal code', { exact: true }).fill('10001');
  const preview = page.waitForResponse(r => r.url() === `${api}/preview` && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Review totals', exact: true }).click();
  expect((await preview).status()).toBe(200);
  await expect(page.getByRole('main')).toContainText('$31.40');
  const started = page.waitForResponse(r => r.url() === `${api}/checkout` && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Start test checkout', exact: true }).click();
  const response = await started;
  expect(response.status()).toBe(201);
  const checkout = await response.json();
  expect(checkout).toMatchObject({ state: 'ready', amount_minor: '3140', currency: 'USD' });
  await expect(page.getByRole('button', { name: 'Fake payment: capture', exact: true })).toBeEnabled();
  await expect(page.getByRole('link', { name: 'View order confirmation', exact: true })).toHaveCount(0);
  return checkout;
}

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

async function registerAndLogin(page: Page, email: string) {
  const password='CI-only-c09f2-password!';
  await page.goto(`${fixture.storefront}/account`);
  await expect(page.locator('[data-customer-ready]')).toBeVisible();
  await page.getByLabel('Email', {exact:true}).fill(email);
  await page.getByLabel('Password', {exact:true}).fill(password);
  await page.getByRole('button',{name:'Register',exact:true}).click();
  await expect(page.getByRole('status')).toContainText('verification email has been sent');
  let messageId='';
  await expect.poll(async()=>{
    const listing=await page.request.get('http://127.0.0.1:8025/api/v1/messages?limit=50');
    const data=await listing.json();
    messageId=data.messages.find((m:{ID:string;To:{Address:string}[]})=>m.To.some(to=>to.Address===email))?.ID??'';
    return messageId;
  }).not.toBe('');
  expect(messageId).toMatch(/^[A-Za-z0-9-]{1,128}$/);
  const message=await (await page.request.get(`http://127.0.0.1:8025/api/v1/message/${messageId}`)).json();
  const proof=new URL(message.Text.trim().split('\n').at(-1));
  expect(proof.origin).toBe('https://store.e2e.test');expect(proof.pathname).toBe('/customer/verify');
  expect(new URLSearchParams(proof.hash.slice(1)).get('tenant')).toBe(fixture.tenantId);
  await page.goto(`${fixture.storefront}/customer/verify${proof.hash}`);
  await expect(page.getByRole('button',{name:'Verify email',exact:true})).toBeEnabled();
  await expect(page).toHaveURL(`${fixture.storefront}/customer/verify`);
  await page.getByRole('button',{name:'Verify email',exact:true}).click();
  await expect(page.getByRole('status').filter({hasText:'Email verified.'})).toBeVisible();
  await page.goto(`${fixture.storefront}/account`);
  await page.getByLabel('Email',{exact:true}).fill(email);await page.getByLabel('Password',{exact:true}).fill(password);
  await page.getByRole('button',{name:'Sign in',exact:true}).click();
  await expect(page.getByRole('link',{name:'Order history',exact:true})).toBeVisible();
}

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
  await audit(page,info);await info.attach('history-list',{body:await page.screenshot({fullPage:true}),contentType:'image/png'});
  await page.getByRole('button',{name:`Order #${history.items[0].order_number}`,exact:true}).click();
  await expect(page.getByRole('region',{name:'Order details'})).toContainText('C24-CANVAS-TOTE');
  await audit(page,info);await info.attach('history-detail',{body:await page.screenshot({fullPage:true}),contentType:'image/png'});
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
