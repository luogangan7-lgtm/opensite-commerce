/** Responsibility: approved C-09c fixture purchasing, privacy and page accessibility acceptance.
 * Dependencies: actual published Go read API, Node Astro BFF, synthetic upstream and Playwright/axe.
 * Invariants: fixture proofs never imply production checkout/auth/payment acceptance; no local golden updates.
 */
import { expect, test, type Page, type APIRequestContext } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
const site = 'http://127.0.0.1:4323';
const fixture = 'http://127.0.0.1:4176';
const tenant = 'c0900001-0000-4000-8000-000000000001';
const sku = 'c09c0001-0000-4000-8000-000000000001';
const api = `${site}/purchase/api`;
const cspFailures = new Map<Page, string[]>();
test.describe('storefront purchasing pages', () => {
test.beforeEach(async ({ page }) => {
  const failures: string[] = []; cspFailures.set(page, failures);
  page.on('console', message => { if (message.text().startsWith('commerce-csp:')) failures.push(message.text()); });
  await page.addInitScript(() => document.addEventListener('securitypolicyviolation', event => console.error(`commerce-csp:${event.violatedDirective}`)));
});
test.afterEach(async ({ page }) => { expect(cspFailures.get(page) ?? []).toEqual([]); cspFailures.delete(page); });
async function quality(page: Page, name: string) {
  await page.evaluate(async () => { await document.fonts.ready; });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  const report = await new AxeBuilder({ page }).analyze();
  expect(report.violations.filter(item => ['serious', 'critical'].includes(item.impact ?? ''))).toEqual([]);
  if (process.env.GITHUB_ACTIONS === 'true') await expect(page).toHaveScreenshot(`storefront-commerce-${name}.png`, { fullPage: true });
}
async function add(page: Page) {
  await page.goto(`${site}/products/canvas-tote`);
  expect((await page.context().cookies(site)).some(cookie => cookie.name === '__Host-osc_storefront_fixture' && cookie.httpOnly && cookie.secure)).toBe(true);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Canvas tote');
  await expect(page.locator('[data-fixture-notice]')).toBeVisible();
  await page.getByRole('button', { name: 'Add to cart', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('Item added to cart.');
  await page.goto(`${site}/cart`);
  await expect(page.locator('[data-cart-line]')).toHaveCount(1);
}
async function review(page: Page) {
  await page.goto(`${site}/checkout`);
  await expect(page.getByLabel('Country code', { exact: true })).toBeVisible();
  if (await page.getByLabel('Email', { exact: true }).count()) await page.getByLabel('Email', { exact: true }).fill('buyer@example.test');
  if (await page.getByLabel('Full name', { exact: true }).count()) await page.getByLabel('Full name', { exact: true }).fill('Fixture buyer');
  await page.getByLabel('Country code', { exact: true }).fill('US');
  await page.getByLabel('Address line 1', { exact: true }).fill('1 Test Way');
  await page.getByLabel('City', { exact: true }).fill('Test City');
  await page.getByLabel('Postal code', { exact: true }).fill('10001');
  await page.getByRole('button', { name: 'Review totals', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Start test checkout', exact: true })).toBeEnabled();
}
async function start(page: Page) {
  await page.getByRole('button', { name: 'Start test checkout', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Fake payment: capture', exact: true })).toBeVisible();
}
async function csrf(page: Page) {
  return page.evaluate(async () => (await (await fetch('/purchase/api/context')).json()).csrf as string);
}

test('commerce fixture product SKU selection and purchasing accessibility', async ({ page }) => {
  await page.goto(`${site}/products/canvas-tote`);
  await expect(page.getByRole('button', { name: 'Add to cart', exact: true })).toBeEnabled();
  await expect(page.getByLabel('Variant', { exact: true })).toBeVisible();
  await quality(page, 'product');
});

test('commerce fixture checkout address validation remains accessible', async ({ page }) => {
  await add(page); await page.goto(`${site}/checkout`);
  await expect(page.getByRole('button', { name: 'Review totals', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Review totals', exact: true }).click();
  await expect(page.getByLabel('Country code', { exact: true })).toHaveAttribute('aria-invalid', 'true');
  await quality(page, 'checkout-invalid');
});

test('commerce fixture guest cart → API totals → explicit Fake capture → confirmation', async ({ page, context }) => {
  const bodies: string[] = [];
  page.on('response', async response => { if (response.url().startsWith(api) && response.headers()['content-type']?.includes('json')) bodies.push(await response.text()); });
  await add(page);
  await page.getByLabel('Quantity', { exact: true }).fill('2');
  await page.getByRole('button', { name: 'Update quantity', exact: true }).click();
  await expect(page.getByRole('main')).toContainText('$48.00');
  await quality(page, 'cart');
  const cookies = await context.cookies(site);
  const session = cookies.find(cookie => cookie.name === '__Host-osc_storefront_fixture');
  expect(session).toMatchObject({ secure: true, httpOnly: true, sameSite: 'Lax', path: '/' });
  expect(await page.evaluate(() => document.cookie)).not.toContain('__Host-osc_storefront_fixture');
  await review(page);
  await expect(page.getByRole('main')).toContainText('$57.80');
  await quality(page, 'checkout');
  await start(page);
  await expect(page.getByRole('link', { name: 'View order confirmation', exact: true })).toHaveCount(0);
  await page.getByRole('button', { name: 'Fake payment: capture', exact: true }).click();
  await page.getByRole('link', { name: 'View order confirmation', exact: true }).click();
  await expect(page).toHaveURL(`${site}/order-complete`);
  await expect(page.locator('[data-completion-ready]')).toBeVisible();
  await expect(page.getByRole('main')).toContainText('$57.80');
  await quality(page, 'completion');
  const html = await page.content();
  const storage = await page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage } }));
  for (const value of [...bodies, html, storage, page.url()]) {
    expect(value).not.toMatch(/cart_token|X-Cart-Token|X-Fixture-Session/i);
    expect(value).not.toContain(session!.value);
  }
});

test('completion requires paid state as well as captured payment', async ({ page }) => {
  await add(page); await review(page); await start(page);
  await page.route(`${api}/completion`, route => route.fulfill({json:{checkout_id:'c09c0010-0000-4000-8000-000000000001',order_id:'c09c0011-0000-4000-8000-000000000001',state:'pending_payment',amount_minor:'3140',currency:'USD',order_version:'1',payment:{provider_ref:'fake_test',status:'captured'}}}));
  await page.goto(`${site}/order-complete`);
  await expect(page.locator('[data-completion-page]')).toHaveAttribute('aria-busy','false');
  await expect(page.locator('[data-completion-ready]')).toHaveCount(0);
});

test('commerce fixture absolute quantities, zero removes, stale version recovers', async ({ page, request }) => {
  await add(page);
  const ownCart = await page.evaluate(async () => (await (await fetch('/purchase/api/cart')).json()).cart);
  const control = await request.post(`${fixture}/__fixture/scenario`, { data: { cart_id: ownCart.id, action: 'conflict' } });
  expect(control.status()).toBe(200);
  await page.getByLabel('Quantity', { exact: true }).fill('3');
  const stale = page.waitForResponse(response => response.url().includes('/purchase/api/lines/') && response.request().method() === 'PUT');
  await page.getByRole('button', { name: 'Update quantity', exact: true }).click();
  expect((await stale).status()).toBe(412);
  await expect(page.getByRole('status')).toContainText(/changed|refresh|updated/i);
  await expect(page.getByLabel('Quantity', { exact: true })).toHaveValue('2');
  await page.getByLabel('Quantity', { exact: true }).fill('3');
  await page.getByRole('button', { name: 'Update quantity', exact: true }).click();
  await expect(page.getByRole('main')).toContainText('$72.00');
  await page.getByRole('button', { name: 'Remove', exact: true }).click();
  await expect(page.locator('[data-cart-line]')).toHaveCount(0);
});

test('stale Start is rejected before intent and refreshed money requires another explicit consent',async({page,request})=>{
  await add(page);await review(page);
  const ownCart=await page.evaluate(async()=>(await(await fetch('/purchase/api/cart')).json()).cart);
  expect((await request.post(`${fixture}/__fixture/scenario`,{data:{cart_id:ownCart.id,action:'conflict'}})).status()).toBe(200);
  const stale=page.waitForResponse(r=>r.url()===`${api}/checkout`);
  await page.getByRole('button',{name:'Start test checkout',exact:true}).click();
  expect((await stale).status()).toBe(409);
  await expect(page.getByRole('button',{name:'Confirm updated total',exact:true})).toBeVisible();
  await expect(page.getByRole('button',{name:'Start test checkout',exact:true})).toBeDisabled();
  await expect(page.getByRole('button',{name:'Fake payment: capture',exact:true})).toHaveCount(0);
  await page.getByRole('button',{name:'Confirm updated total',exact:true}).click();
  await start(page);
  await page.getByRole('button',{name:'Fake payment: capture',exact:true}).click();
  await expect(page.getByRole('link',{name:'View order confirmation',exact:true})).toBeVisible();
});

test('checkout requires confirmation of an updated server total and reuses ambiguous submission keys', async ({ page }) => {
  await add(page); await review(page);
  const keys: string[] = [];
  await page.route(`${api}/checkout`, async route => {
    keys.push(route.request().headers()['idempotency-key']!);
    const response = await route.fetch();
    if (keys.length === 1) {
      // The backend accepted this request, but the browser received an ambiguous failure.
      await route.fulfill({status:502,contentType:'application/problem+json',body:JSON.stringify({code:'purchase_request_failed'})});
    } else {
      const body=await response.json();
      await route.fulfill({response,json:{...body,amount_minor:'9999'}});
    }
  });
  await page.getByRole('button',{name:'Start test checkout',exact:true}).click();
  await expect(page.getByRole('alert')).toBeVisible();
  await page.getByRole('button',{name:'Start test checkout',exact:true}).click();
  await expect(page.getByRole('button',{name:'Fake payment: capture',exact:true})).toBeDisabled();
  await expect(page.locator('.payment-result')).toContainText('$99.99');
  expect(keys).toHaveLength(2); expect(keys[0]).toBeTruthy(); expect(keys[1]).toBe(keys[0]);
  let accepted: unknown;
  await page.route(`${api}/payment`, async route => { accepted=route.request().postDataJSON().accepted_total; await route.fulfill({status:409,json:{code:'checkout_total_changed'}}); });
  await page.getByRole('button',{name:'Confirm updated total',exact:true}).click();
  await expect(page.getByRole('button',{name:'Fake payment: capture',exact:true})).toBeEnabled();
  await page.getByRole('button',{name:'Fake payment: capture',exact:true}).click();
  await expect.poll(()=>accepted).toEqual({amount_minor:'9999',currency:'USD'});
});

for (const outcome of ['fail', 'cancel'] as const) test(`commerce fixture Fake ${outcome} never opens paid confirmation`, async ({ page }) => {
  await add(page); await review(page); await start(page);
  await page.getByRole('button', { name: `Fake payment: ${outcome}`, exact: true }).click();
  await expect(page.getByRole('link', { name: 'View order confirmation', exact: true })).toHaveCount(0);
  await page.goto(`${site}/order-complete`);
  await expect(page.locator('[data-completion-page]')).toHaveAttribute('aria-busy', 'false');
  await expect(page.locator('[data-completion-ready]')).toHaveCount(0);
  await expect(page.getByRole('main')).not.toContainText(/order confirmed|payment received/i);
});

test('commerce fixture session isolation, cross Origin and missing CSRF reject mutations', async ({ page, browser }) => {
  await add(page);
  const token = await csrf(page);
  const blocked = await page.request.post(`${api}/cart`, { headers: { Origin: 'https://foreign.example', 'X-CSRF-Token': token }, data: { sku_id: sku, quantity: 9 } });
  expect(blocked.status()).toBe(403);
  const missing = await page.request.post(`${api}/cart`, { headers: { Origin: site }, data: { sku_id: sku, quantity: 9 } });
  expect(missing.status()).toBe(403);
  const other = await browser.newContext();
  try {
    const stranger = await other.newPage(); await stranger.goto(`${site}/order-complete`);
    await expect(stranger.getByRole('main')).not.toContainText(/order confirmed|payment received/i);
    await stranger.goto(`${site}/cart`); await expect(stranger.locator('[data-cart-line]')).toHaveCount(0);
  } finally { await other.close(); }
});

test('commerce fixture customer and registration explicitly remain synthetic', async ({ page }) => {
  await page.goto(`${site}/account`);
  await expect(page.locator('[data-fixture-notice]')).toBeVisible();
  await quality(page, 'account');
  await page.getByLabel('Email', { exact: true }).fill('customer@example.test');
  await page.getByLabel('Password', { exact: true }).fill('fixture-password');
  await page.getByRole('button', { name: 'Register', exact: true }).click();
  await expect(page.getByRole('main')).toContainText(/verification|verify/i);
  await page.getByLabel('Password', { exact: true }).fill('fixture-password');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Sign out', exact: true })).toBeVisible();
  await add(page); await review(page);
  await expect(page.getByLabel('Email', { exact: true })).toHaveCount(0);
  const customerWrite = page.waitForRequest(request => request.url() === `${api}/checkout` && request.method() === 'POST');
  await start(page);
  expect((await customerWrite).postDataJSON()).not.toHaveProperty('buyer_contact');
  await page.getByRole('button', { name: 'Fake payment: capture', exact: true }).click();
  await page.getByRole('link', { name: 'View order confirmation', exact: true }).click();
  await expect(page.locator('[data-completion-ready]')).toBeVisible();
  await page.goto(`${site}/account`);
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeVisible();
});

test('mailbox proof is cleared from the URL and never rendered or submitted by GET',async({page})=>{
  const token='Z'.repeat(43),writes:string[]=[];
  page.on('request',request=>{if(request.method()==='POST')writes.push(request.url());});
  const response=await page.goto(`${site}/customer/verify#token=${token}`);
  expect(response?.headers()['cache-control']).toContain('no-store');
  expect(response?.headers()['referrer-policy']).toBe('no-referrer');
  await expect(page.getByRole('button',{name:'Verify email',exact:true})).toBeEnabled();
  expect(new URL(page.url()).hash).toBe('');
  expect(await page.locator('body').innerHTML()).not.toContain(token);
  expect(writes).toEqual([]);
  const report=await new AxeBuilder({page}).analyze();
  expect(report.violations.filter(item=>['serious','critical'].includes(item.impact??''))).toEqual([]);
});

test('production storefront keeps purchasing disabled and makes no fixture requests', async ({ page }) => {
  const calls: string[] = [];
  page.on('request', request => { if (request.url().includes('4176') || request.url().includes('/purchase/api/')) calls.push(request.url()); });
  await page.goto('http://127.0.0.1:4321/products/canvas-tote');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Canvas tote');
  await expect(page.getByRole('button', { name: 'Add to cart', exact: true })).toHaveCount(0);
  await expect(page.locator('[data-fixture-notice]')).toHaveCount(0);
  expect(calls).toEqual([]);
});

});

// This upstream probe independently pins capability ownership, strong ETags and idempotency.
async function upstream(request: APIRequestContext, session: string, path: string, method = 'GET', data?: unknown, extra: Record<string, string> = {}) {
  return request.fetch(`${fixture}${path}`, { method, data, headers: { 'X-Fixture-Session': session, ...extra } });
}
test('commerce upstream contract: private owner, If-Match, API pricing and idempotency', async ({ request }) => {
  const owner = 'a'.repeat(43), foreign = 'b'.repeat(43), prefix = `/api/v1/t/${tenant}`;
  // Unique owner session per inherited viewport/run; no shared global reset.
  const unique = `${owner.slice(0, 10)}${crypto.randomUUID().replaceAll('-', '')}x`;
  const created = await upstream(request, unique, `${prefix}/carts`, 'POST', { currency: 'USD' });
  expect(created.status()).toBe(201);
  const raw = await created.json(), cartPath = `${prefix}/carts/${raw.cart.id}`;
  const headers = { 'X-Cart-Token': raw.cart_token, 'If-Match': created.headers().etag };
  expect(headers['If-Match']).toMatch(/^"v[0-9]+-[a-f0-9]{64}"$/);
  expect((await upstream(request, foreign, cartPath, 'GET', undefined, headers)).status()).toBe(404);
  expect((await upstream(request, unique, cartPath)).status()).toBe(404);
  const updated = await upstream(request, unique, `${cartPath}/lines/${sku}`, 'PUT', { quantity: 2 }, headers);
  expect(updated.status()).toBe(200); expect((await updated.json()).subtotal).toBe(4800);
  const zero = await upstream(request, unique, `${cartPath}/lines/${sku}`, 'PUT', { quantity: 0 }, { ...headers, 'If-Match': updated.headers().etag });
  expect(zero.status()).toBe(200); expect((await zero.json()).lines).toEqual([]);
  const restored = await upstream(request, unique, `${cartPath}/lines/${sku}`, 'PUT', { quantity: 2 }, { ...headers, 'If-Match': zero.headers().etag });
  expect(restored.status()).toBe(200);
  expect((await upstream(request, unique, `${cartPath}/lines/${sku}`, 'PUT', { quantity: 3 }, headers)).status()).toBe(412);
  const shipping_address = { country: 'US', address1: '1 Test Way', city: 'Test City', postal_code: '10001' }, input = { cart_id: raw.cart.id, shipping_address, buyer_contact: { email: 'buyer@example.test' }, accepted_total: { amount_minor: '5780', currency: 'USD' } };
  const preview = await upstream(request, unique, `${prefix}/checkout/preview`, 'POST', input, headers);
  expect(await preview.json()).toMatchObject({ subtotal_minor: '4800', tax_minor: '480', shipping_minor: '500', total_minor: '5780' });
  const checkoutHeaders = { ...headers, 'Idempotency-Key': `test-${crypto.randomUUID()}` };
  expect((await upstream(request, unique, `${prefix}/checkout`, 'POST', { ...input, shipping_address: { country: 'US', postal_code: '10001' } }, checkoutHeaders)).status()).toBe(422);
  expect((await upstream(request, unique, `${prefix}/checkout`, 'POST', { ...input, buyer_contact: { ...input.buyer_contact, name: 'x'.repeat(201) } }, checkoutHeaders)).status()).toBe(422);
  const checkout = await upstream(request, unique, `${prefix}/checkout`, 'POST', input, checkoutHeaders);
  const result = await checkout.json(); expect(checkout.status()).toBe(201); expect(result).toMatchObject({ state: 'ready', amount_minor: 5780 });
  expect(await (await upstream(request, unique, `${prefix}/checkout`, 'POST', input, checkoutHeaders)).json()).toEqual(result);
  expect((await upstream(request, unique, `${prefix}/checkout`, 'POST', { ...input, shipping_address: { ...shipping_address, country: 'CA' } }, checkoutHeaders)).status()).toBe(422);
  expect((await upstream(request, foreign, '/__fixture/payment', 'POST', { checkout_id: result.checkout_id, outcome: 'captured' }, headers)).status()).toBe(404);
  expect((await upstream(request, unique, '/__fixture/payment', 'POST', { checkout_id: result.checkout_id, outcome: 'captured' }, headers)).status()).toBe(200);
  expect(await (await upstream(request, unique, '/__fixture/completion', 'GET', undefined, headers)).json()).toMatchObject({ state: 'paid' });
  const current = await upstream(request, unique, cartPath, 'GET', undefined, headers);
  const emptied = await upstream(request, unique, `${cartPath}/lines/${sku}`, 'DELETE', undefined, { ...headers, 'If-Match': current.headers().etag });
  expect(emptied.status()).toBe(200); expect((await emptied.json()).lines).toEqual([]);
  expect(await (await upstream(request, unique, `${prefix}/checkout`, 'POST', input, checkoutHeaders)).json()).toEqual(result);
});
