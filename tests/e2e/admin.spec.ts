/** Responsibility: exercise the real staff login, guarded catalog and logout, and deny anonymous private operations.
 * Dependencies: production admin static build, same-origin proxy to the actual API, owner from cmd/bootstrap-admin.
 * Invariants: no simulated identity, cookie fabrication or API interception; the session cookie is only observed.
 */
import { expect, test, type Page } from '@playwright/test';
import { assertPublicOnly, audit, fixture } from './fixture';

const sessionCookie = '__Host-osc_admin_session';
const hasSession = async (page: Page) =>
  (await page.context().cookies()).some(cookie => cookie.name === sessionCookie);

test('real admin login rejects malformed and unknown credentials without a session', async ({ page }, info) => {
  const session = page.waitForResponse(response =>
    response.url() === `${fixture.admin}/api/v1/auth/session` && response.request().method() === 'GET');
  await page.goto(`${fixture.admin}/login`);
  expect((await session).status()).toBe(401);
  await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
  await audit(page, info);

  // Native form validation rejects a malformed email before an auth attempt.
  let loginAttempts = 0;
  page.on('request', request => {
    if (request.url() === `${fixture.admin}/api/v1/auth/login` && request.method() === 'POST') loginAttempts++;
  });
  await page.getByLabel('Email', { exact: true }).fill('invalid-email');
  await page.getByLabel('Password', { exact: true }).fill(crypto.randomUUID());
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  expect(await page.getByLabel('Email', { exact: true }).evaluate(element =>
    (element as HTMLInputElement).validity.typeMismatch)).toBe(true);
  expect(loginAttempts).toBe(0);

  await page.getByLabel('Email', { exact: true }).fill('c24-nobody@example.test');
  const rejected = page.waitForResponse(response =>
    response.url() === `${fixture.admin}/api/v1/auth/login` && response.request().method() === 'POST');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  expect((await rejected).status()).toBe(401);
  await expect(page.getByRole('alert')).toHaveText('Unable to sign in. Check your credentials or try again later.');
  await expect(page.getByRole('alert')).toBeFocused();
  await expect(page.getByLabel('Password', { exact: true })).toHaveValue('');
  expect(loginAttempts).toBe(1);
  expect(await hasSession(page)).toBe(false);
  assertPublicOnly(await page.content());
  await audit(page, info);
});

test('real guarded catalog route sends anonymous visitors to login without private reads', async ({ page }, info) => {
  const privateReads: string[] = [];
  page.on('request', request => {
    if (new URL(request.url()).pathname.startsWith('/api/v1/t/')) privateReads.push(request.url());
  });
  await page.goto(`${fixture.admin}/t/${fixture.ownerTenantId}/products`);
  await expect(page).toHaveURL(`${fixture.admin}/login`);
  await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
  expect(privateReads).toEqual([]);
  await expect(page.getByRole('link', { name: 'Create product', exact: true })).toHaveCount(0);
  assertPublicOnly(await page.content());
  await audit(page, info);
});

test('real bootstrapped owner logs in, holds a session and signs out', async ({ page }, info) => {
  await page.goto(`${fixture.admin}/login`);
  await page.getByLabel('Email', { exact: true }).fill(fixture.ownerEmail);
  await page.getByLabel('Password', { exact: true }).fill(fixture.ownerPassword);
  const login = page.waitForResponse(response =>
    response.url() === `${fixture.admin}/api/v1/auth/login` && response.request().method() === 'POST');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  expect((await login).status()).toBe(201);
  await expect(page.getByRole('heading', { name: 'Open a store', exact: true })).toBeVisible();
  expect(await hasSession(page)).toBe(true);
  await audit(page, info);

  // The real session endpoint recognises the cookie that the browser holds.
  const me = await page.request.get(`${fixture.admin}/api/v1/auth/session`, { maxRedirects: 0 });
  expect(me.status()).toBe(200);
  expect(await me.json()).toMatchObject({ user_id: expect.any(String), session_id: expect.any(String) });
  assertPublicOnly(await page.content());

  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await expect(page).toHaveURL(`${fixture.admin}/login`);
  expect(await hasSession(page)).toBe(false);
  expect((await page.request.get(`${fixture.admin}/api/v1/auth/session`, { maxRedirects: 0 })).status()).toBe(401);
});

test('real API denies anonymous catalog reads and creation before data access', async ({ request }) => {
  const path = `${fixture.api}/api/v1/t/${fixture.tenantId}/products`;
  for (const response of [
    await request.get(path, { maxRedirects: 0 }),
    await request.get(`${path}/${fixture.productId}`, { maxRedirects: 0 }),
    await request.post(path, { data: { product_code: 'C24-UNAUTHORIZED' }, maxRedirects: 0 }),
  ]) {
    expect(response.status()).toBe(401);
    expect(response.headers()['content-type']).toContain('application/problem+json');
    const text = await response.text();
    const problem = JSON.parse(text);
    expect(problem).toMatchObject({ status: 401, code: 'unauthenticated' });
    expect(problem).not.toHaveProperty('id');
    expect(problem).not.toHaveProperty('product_code');
    expect(problem).not.toHaveProperty('items');
    expect(text).not.toContain('C24-UNAUTHORIZED');
    assertPublicOnly(text);
  }
});


test('real owner session reads and creates tenant catalog through production authorization', async ({ page }, info) => {
  test.setTimeout(150_000);
  await page.goto(`${fixture.admin}/login`);
  await page.getByLabel('Email', { exact: true }).fill(fixture.ownerEmail);
  await page.getByLabel('Password', { exact: true }).fill(fixture.ownerPassword);
  const login = page.waitForResponse(response =>
    response.url() === `${fixture.admin}/api/v1/auth/login` && response.request().method() === 'POST');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  expect((await login).status()).toBe(201);
  await expect(page.getByRole('heading', { name: 'Open a store', exact: true })).toBeVisible();

  const catalogURL = `${fixture.admin}/api/v1/t/${fixture.ownerTenantId}/products`;
  const catalog = page.waitForResponse(response =>
    response.url().startsWith(catalogURL) && response.request().method() === 'GET');
  await page.getByLabel('Store ID', { exact: true }).fill(fixture.ownerTenantId);
  await page.getByRole('button', { name: 'Open store', exact: true }).click();
  expect((await catalog).status()).toBe(200);
  await expect(page.getByRole('heading', { name: 'Products', exact: true })).toBeVisible();
  await audit(page, info);

  // Observe the real issued cookie, without modifying it or calling /auth/session.
  // This fixture uses the existing minimum idle lifetime (1m). Tenant activity
  // must keep both DB authority and the browser cookie alive past its first expiry.
  const initialCookie = (await page.context().cookies()).find(cookie => cookie.name === sessionCookie);
  expect(initialCookie, 'Owner login must issue a browser session cookie').toBeDefined();
  const initialExpiry = initialCookie!.expires;
  const remainingLifetime = initialExpiry - Date.now() / 1000;
  expect(remainingLifetime, 'CI must use the explicit one-minute idle fixture').toBeGreaterThan(0);
  expect(remainingLifetime, 'CI must use the explicit one-minute idle fixture').toBeLessThanOrEqual(65);
  const activityStatuses: number[] = [];
  const browserCatalogStatus = () => page.evaluate(async url =>
    (await fetch(url, { credentials: 'same-origin', cache: 'no-store' })).status, catalogURL);
  const pastOriginalExpiry = (initialExpiry + 2) * 1000;
  while (Date.now() < pastOriginalExpiry) {
    const status = await browserCatalogStatus();
    activityStatuses.push(status);
    expect(status, 'Active tenant reads must keep the browser authenticated').toBe(200);
    await page.waitForTimeout(2_000);
  }
  const finalStatus = await browserCatalogStatus();
  expect(finalStatus, 'Browser tenant request after the original cookie expiry must remain admitted').toBe(200);
  const renewedCookie = (await page.context().cookies()).find(cookie => cookie.name === sessionCookie);
  expect(renewedCookie, 'Tenant activity must preserve the browser cookie').toBeDefined();
  expect(renewedCookie!.expires).toBeGreaterThan(initialExpiry + 2);
  expect(renewedCookie!.expires).toBeGreaterThan(Date.now() / 1000);
  await info.attach('tenant-activity-cookie-lifetime', {
    body: JSON.stringify({ initialExpiry, renewedExpiry: renewedCookie!.expires, activityStatuses, finalStatus }),
    contentType: 'application/json',
  });

  // The same real cookie cannot read another tenant, or write without session CSRF.
  expect((await page.request.get(`${fixture.admin}/api/v1/t/${fixture.tenantId}/products`)).status()).toBe(404);
  expect((await page.request.post(catalogURL, { data: { product_code: 'CSRF-REJECTED' } })).status()).toBe(403);
  await page.getByRole('link', { name: 'Create product', exact: true }).click();
  const productCode = `C02C-${crypto.randomUUID()}`.toUpperCase();
  await page.getByLabel('Product code', { exact: true }).fill(productCode);
  const created = page.waitForResponse(response =>
    response.url() === catalogURL && response.request().method() === 'POST');
  await page.getByRole('button', { name: 'Create product', exact: true }).click();
  const response = await created;
  expect(response.status()).toBe(201);
  const product = await response.json();
  expect(product).toMatchObject({ product_code: productCode });
  await expect(page).toHaveURL(`${fixture.admin}/t/${fixture.ownerTenantId}/products/${product.id}`);
  await expect(page.getByLabel('Product code', { exact: true })).toHaveValue(productCode);
  const persisted = await page.request.get(`${catalogURL}/${product.id}`);
  expect(persisted.status()).toBe(200);
  expect(await persisted.json()).toMatchObject({ id: product.id, product_code: productCode });
  assertPublicOnly(await page.content());
  await audit(page, info);
});
