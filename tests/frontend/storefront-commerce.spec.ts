/** Responsibility: approved C-09c fixture purchasing, privacy and page accessibility acceptance.
 * Dependencies: actual published Go read API, Node Astro BFF, synthetic upstream and Playwright/axe.
 * Invariants: fixture proofs never imply production checkout/auth/payment acceptance; no local golden updates.
 */
import { expect, test, type Page, type APIRequestContext } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
const site = 'http://127.0.0.1:4323';
const realSite = 'http://127.0.0.1:4324';
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
// Hold a routed response until its production loading UI has been inspected; never use timing sleeps.
async function heldResponse(page: Page, url: string, json: unknown, status = 200) {
  let release!: () => void, entered!: () => void;
  const pending = new Promise<void>(resolve => { release = resolve; });
  const started = new Promise<void>(resolve => { entered = resolve; });
  await page.route(url, async route => { entered(); await pending; await route.fulfill({ status, json }); }, { times: 1 });
  return { started, release };
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
  await quality(page, 'completion-pending-captured');
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
  await quality(page, 'checkout-stale-total-consent-disabled');
  await page.getByRole('button',{name:'Confirm updated total',exact:true}).click();
  await expect(page.getByRole('button',{name:'Start test checkout',exact:true})).toBeEnabled();
  await expect(page.getByRole('button',{name:'Confirm updated total',exact:true})).toHaveCount(0);
  await quality(page, 'checkout-stale-total-consent-confirmed');
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
  await quality(page, 'checkout-updated-payment-consent-disabled');
  expect(keys).toHaveLength(2); expect(keys[0]).toBeTruthy(); expect(keys[1]).toBe(keys[0]);
  let accepted: unknown;
  await page.route(`${api}/payment`, async route => { accepted=route.request().postDataJSON().accepted_total; await route.fulfill({status:409,json:{code:'checkout_total_changed'}}); });
  await page.getByRole('button',{name:'Confirm updated total',exact:true}).click();
  await expect(page.getByRole('button',{name:'Fake payment: capture',exact:true})).toBeEnabled();
  await quality(page, 'checkout-updated-payment-consent-confirmed');
  await page.getByRole('button',{name:'Fake payment: capture',exact:true}).click();
  await expect.poll(()=>accepted).toEqual({amount_minor:'9999',currency:'USD'});
});

test('changed currency and int64 server total remain gated by explicit consent', async ({ page }) => {
  await add(page); await review(page);
  await page.route(`${api}/checkout`, async route => {
    const response = await route.fetch();
    expect(response.status()).toBe(201);
    await route.fulfill({ response, json: { ...await response.json(), amount_minor: '9223372036854775807', currency: 'JPY' } });
  });
  await page.getByRole('button', { name: 'Start test checkout', exact: true }).click();
  const capture = page.getByRole('button', { name: 'Fake payment: capture', exact: true });
  await expect(capture).toBeDisabled();
  await expect(page.locator('.payment-result')).toContainText('9,223,372,036,854,775,807');
  await quality(page, 'checkout-long-changed-currency-disabled');
  await page.getByRole('button', { name: 'Confirm updated total', exact: true }).click();
  await expect(capture).toBeEnabled();
  await quality(page, 'checkout-long-changed-currency-confirmed');
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

// These routes drive shipped components with safe DTOs. They prove UI states, not backend identity/payment acceptance.
for (const outcome of ['empty', 'error', 'long-money'] as const) test(`routed completion ${outcome} visual states`, async ({ page }) => {
  const response = outcome === 'empty' ? null : outcome === 'error' ? { code: 'purchase_request_failed' } : {
    checkout_id: 'c09c0010-0000-4000-8000-000000000001', order_id: 'c09c0011-0000-4000-8000-000000000001',
    state: 'paid', amount_minor: '9223372036854775807', currency: 'USD', order_version: '1',
    payment: { provider_ref: 'visual-test', status: 'captured' }
  };
  const gate = await heldResponse(page, `${api}/completion`, response, outcome === 'error' ? 503 : 200);
  await page.goto(`${site}/order-complete`); await gate.started;
  try {
    await expect(page.locator('[data-completion-page]')).toHaveAttribute('aria-busy', 'true');
    if (outcome === 'empty') await quality(page, 'completion-loading');
  } finally { gate.release(); }
  await expect(page.locator('[data-completion-page]')).toHaveAttribute('aria-busy', 'false');
  if (outcome === 'long-money') {
    await expect(page.locator('[data-completion-ready]')).toContainText('$92,233,720,368,547,758.07');
  } else {
    await expect(page.locator('[data-completion-ready]')).toHaveCount(0);
    await expect(outcome === 'error' ? page.getByRole('alert') : page.getByRole('heading', { name: 'No completed order', exact: true })).toBeVisible();
  }
  await quality(page, `completion-${outcome}`);
});

test('routed real-mode mailbox verification disabled state contains no proof', async ({ page }) => {
  await page.goto(`${realSite}/customer/verify`);
  await expect(page.getByRole('button', { name: 'Verify email', exact: true })).toBeDisabled();
  await quality(page, 'verification-disabled');
});
test('routed real-mode account registration session and sign-out loading', async ({ page }) => {
  await page.goto(`${realSite}/account`);
  await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeEnabled();
  await expect(page.locator('[data-fixture-notice]')).toHaveCount(0);
  await expect(page.getByRole('main')).toContainText('Registration requires 12–1024 UTF-8 bytes.');
  await quality(page, 'account-real-long-help');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByLabel('Email', { exact: true })).toHaveAttribute('aria-invalid', 'true');
  await expect(page.getByLabel('Password', { exact: true })).toHaveAttribute('aria-invalid', 'true');
  await quality(page, 'account-real-validation');
  await page.route(`${realSite}/purchase/api/customer/register`, route => route.fulfill({ status: 202, json: { status: 'verification_pending' } }), { times: 1 });
  await page.getByLabel('Email', { exact: true }).fill('visual-registration@example.test');
  await page.getByLabel('Password', { exact: true }).fill('visual-only-password');
  await page.getByRole('button', { name: 'Register', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('Verify your email before signing in.');
  await expect(page.getByLabel('Password', { exact: true })).toHaveValue('');
  await page.getByLabel('Email', { exact: true }).fill('');
  await quality(page, 'account-real-registration-pending');
  let actor = 'customer';
  await page.route(`${realSite}/purchase/api/context`, route => route.fulfill({ json: { enabled: true, fixture: false, actor, csrf: 'visual-test', products: [] } }));
  await page.route(`${realSite}/purchase/api/customer/session`, route => route.fulfill({ json: {} }), { times: 1 });
  await page.getByLabel('Email', { exact: true }).fill('visual-session@example.test');
  await page.getByLabel('Password', { exact: true }).fill('visual-only-password');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Sign out', exact: true })).toBeVisible();
  await expect(page.getByLabel('Password', { exact: true })).toHaveCount(0);
  await expect(page.getByRole('link', { name: 'Order history', exact: true })).toBeVisible();
  await quality(page, 'account-real-signed-in');
  const gate = await heldResponse(page, `${realSite}/purchase/api/customer/session`, {});
  actor = 'guest';
  await page.getByRole('button', { name: 'Sign out', exact: true }).click(); await gate.started;
  try {
    await expect(page.locator('[data-customer-ready]')).toHaveAttribute('aria-busy', 'true');
    await expect(page.locator('[data-customer-ready] button[aria-busy="true"]')).toBeDisabled();
    await quality(page, 'account-real-loading');
  } finally { gate.release(); }
  await expect(page.getByRole('status')).toHaveText('You are signed out.');
  await page.getByLabel('Email', { exact: true }).fill('');
  await expect(page.getByLabel('Password', { exact: true })).toHaveValue('');
  await quality(page, 'account-real-signed-out');
});
test('routed real-mode account error keeps retry available without exposing inputs', async ({ page }) => {
  await page.route(`${realSite}/purchase/api/customer/session`, route => route.fulfill({ status: 503, json: { code: 'purchase_request_failed' } }));
  await page.goto(`${realSite}/account`);
  await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeEnabled();
  await expect(page.locator('[data-fixture-notice]')).toHaveCount(0);
  await page.getByLabel('Email', { exact: true }).fill('visual-error@example.test');
  await page.getByLabel('Password', { exact: true }).fill('visual-only-password');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('alert')).toHaveText('We could not complete this request. Try again.');
  await page.getByLabel('Email', { exact: true }).fill('');
  await page.getByLabel('Password', { exact: true }).fill('');
  await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeEnabled();
  await quality(page, 'account-real-error');
});
test('real-mode account remains explicitly disabled without a configured customer backend', async ({ page }) => {
  await page.goto('http://127.0.0.1:4321/account');
  await expect(page.locator('[data-customer-ready]')).toHaveAttribute('aria-busy', 'false');
  await expect(page.getByRole('button', { name: 'Temporarily unavailable', exact: true })).toBeDisabled();
  await expect(page.getByLabel('Password', { exact: true })).toHaveCount(0);
  await expect(page.locator('[data-fixture-notice]')).toHaveCount(0);
  await quality(page, 'account-real-disabled');
});
test('invalid mailbox fragment is cleared and leaves verification disabled', async ({ page }) => {
  await page.goto(`${realSite}/customer/verify#token=invalid-proof`);
  await expect(page.getByRole('button', { name: 'Verify email', exact: true })).toBeDisabled();
  await expect(page.getByRole('status')).toHaveText('Open the verification link from your email.');
  expect(new URL(page.url()).hash).toBe('');
  expect(await page.content()).not.toContain('invalid-proof');
  await quality(page, 'verification-invalid');
});
for (const outcome of ['success', 'error'] as const) test(`routed real-mode mailbox verification ${outcome}`, async ({ page }) => {
  const origin = realSite, proof = 'V'.repeat(43);
  await page.route(`${origin}/purchase/api/context`, route => route.fulfill({ json: { enabled: true, fixture: false, actor: 'guest', csrf: 'visual-test', products: [] } }));
  const gate = await heldResponse(page, `${origin}/purchase/api/customer/verify`, outcome === 'success' ? { status: 'verified' } : { code: 'purchase_request_failed' }, outcome === 'success' ? 200 : 503);
  await page.goto(`${origin}/customer/verify#token=${proof}`);
  await expect(page.getByRole('button', { name: 'Verify email', exact: true })).toBeEnabled();
  expect(new URL(page.url()).hash).toBe('');
  expect(await page.content()).not.toContain(proof);
  if (outcome === 'success') await quality(page, 'verification-ready');
  await page.getByRole('button', { name: 'Verify email', exact: true }).click(); await gate.started;
  try {
    await expect(page.locator('[data-customer-verification]')).toHaveAttribute('aria-busy', 'true');
    await expect(page.locator('[data-customer-verification] button[aria-busy="true"]')).toBeDisabled();
    if (outcome === 'success') await quality(page, 'verification-loading');
  } finally { gate.release(); }
  await expect(page.locator('[data-customer-verification]')).toHaveAttribute('aria-busy', 'false');
  await expect(page.getByRole('status')).toContainText(outcome === 'success' ? 'Email verified.' : 'Verification could not be completed.');
  await expect(page.getByRole('button', { name: 'Verify email', exact: true })).toBeDisabled();
  expect(await page.content()).not.toContain(proof);
  await quality(page, `verification-${outcome}`);
});

test('routed real-mode order history list detail and pagination visual states', async ({ page }) => {
  const origin = realSite, history = `${origin}/purchase/api/history`;
  const order = { id: 'c09c0011-0000-4000-8000-000000000001', order_number: 'VISUAL-2026-LONG-ORDER-REFERENCE-0000000001', state: 'pending_payment', currency: 'USD', total_minor: '9223372036854775807', created_at: '2026-01-02T12:00:00Z' };
  const first = await heldResponse(page, history, { items: [order], next_cursor: 'visual_page_2' });
  await page.goto(`${origin}/account/orders`); await first.started;
  try {
    await expect(page.getByRole('status')).toHaveText('Loading orders…');
    await quality(page, 'history-loading');
  } finally { first.release(); }
  const orderButton = page.getByRole('button', { name: `Order #${order.order_number}`, exact: true });
  await expect(orderButton).toBeVisible();
  await expect(page.getByRole('button', { name: 'Next orders', exact: true })).toBeEnabled();
  await quality(page, 'history-list-long');
  const detail = await heldResponse(page, `${history}/${order.id}`, { order, shipping_minor: '1000', tax_minor: '2000', lines: [{ line_id: 'visual-line', sku_code: 'LONG-SKU-REFERENCE-FOR-THE-EXTENDED-CANVAS-TOTE-COLLECTION-2026', quantity: '999999', line_total_minor: order.total_minor }] });
  await orderButton.click(); await detail.started;
  try {
    await expect(page.getByRole('status')).toHaveText('Loading orders…');
    await quality(page, 'history-detail-loading');
  } finally { detail.release(); }
  await expect(page.getByRole('region', { name: 'Order details' })).toContainText('$92,233,720,368,547,758.07');
  await quality(page, 'history-detail-long');
  await page.route(history, route => route.fulfill({ json: { items: [order], next_cursor: 'visual_page_2' } }), { times: 1 });
  await page.getByRole('button', { name: 'Back to orders', exact: true }).click();
  await expect(orderButton).toBeVisible();
  const next = await heldResponse(page, `${history}?cursor=visual_page_2`, { items: [] });
  await page.getByRole('button', { name: 'Next orders', exact: true }).click(); await next.started;
  try {
    await expect(page.getByRole('status')).toHaveText('Loading orders…');
    await quality(page, 'history-pagination-loading');
  } finally { next.release(); }
  await expect(page.getByRole('main')).toContainText('No orders yet.');
  await expect(page.getByRole('button', { name: 'Next orders', exact: true })).toHaveCount(0);
  await quality(page, 'history-empty');
});
for (const status of [401, 404, 503]) test(`routed real-mode order history ${status} recovery`, async ({ page }) => {
  const origin = realSite, history = `${origin}/purchase/api/history`;
  await page.route(history, route => route.fulfill({ status, json: { code: 'purchase_request_failed' } }), { times: 1 });
  await page.goto(`${origin}/account/orders`);
  await expect(page.getByRole('alert')).toHaveText(status === 401 ? 'Sign in to view your orders.' : status === 404 ? 'Order not found.' : 'Orders could not be loaded. Try again.');
  await quality(page, `history-error-${status}`);
  await page.route(history, route => route.fulfill({ json: { items: [] } }), { times: 1 });
  await page.getByRole('button', { name: 'Try again', exact: true }).click();
  await expect(page.getByRole('main')).toContainText('No orders yet.');
});
test('routed real-mode history detail and pagination failures remain recoverable', async ({ page }) => {
  const history = `${realSite}/purchase/api/history`, order = { id: 'c09c0011-0000-4000-8000-000000000001', order_number: 'VISUAL-1', state: 'paid', currency: 'USD', total_minor: '3140', created_at: '2026-01-02T12:00:00Z' };
  await page.route(history, route => route.fulfill({ json: { items: [order], next_cursor: 'visual_page_2' } }));
  await page.route(`${history}/${order.id}`, route => route.fulfill({ status: 404, json: { code: 'not_found' } }));
  await page.goto(`${realSite}/account/orders`);
  await page.getByRole('button', { name: 'Order #VISUAL-1', exact: true }).click();
  await expect(page.getByRole('alert')).toHaveText('Order not found.');
  await quality(page, 'history-detail-error');
  await page.getByRole('button', { name: 'Try again', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Order #VISUAL-1', exact: true })).toBeVisible();
  await page.route(`${history}?cursor=visual_page_2`, route => route.fulfill({ status: 503, json: { code: 'purchase_request_failed' } }));
  await page.getByRole('button', { name: 'Next orders', exact: true }).click();
  await expect(page.getByRole('alert')).toHaveText('Orders could not be loaded. Try again.');
  await quality(page, 'history-pagination-error');
  await page.getByRole('button', { name: 'Try again', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Order #VISUAL-1', exact: true })).toBeVisible();
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
