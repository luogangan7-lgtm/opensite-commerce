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

test('real bootstrapped owner logs in, opens the guarded catalog and signs out', async ({ page }, info) => {
  await page.goto(`${fixture.admin}/login`);
  await page.getByLabel('Email', { exact: true }).fill(fixture.ownerEmail);
  await page.getByLabel('Password', { exact: true }).fill(fixture.ownerPassword);
  const login = page.waitForResponse(response =>
    response.url() === `${fixture.admin}/api/v1/auth/login` && response.request().method() === 'POST');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  expect((await login).status()).toBe(200);
  await expect(page.getByRole('heading', { name: 'Open a store', exact: true })).toBeVisible();
  expect(await hasSession(page)).toBe(true);
  await audit(page, info);

  await page.getByLabel('Store ID', { exact: true }).fill(fixture.ownerTenantId);
  const catalog = page.waitForResponse(response =>
    new URL(response.url()).pathname === `/api/v1/t/${fixture.ownerTenantId}/products`);
  await page.getByRole('button', { name: 'Open store', exact: true }).click();
  expect((await catalog).status()).toBe(200);
  await expect(page.getByRole('heading', { name: 'Products', exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Create product', exact: true })).toBeVisible();
  // The owner's tenant is separate from the storefront seed: none of its catalog may appear.
  await expect(page.getByText(fixture.productName, { exact: true })).toHaveCount(0);
  assertPublicOnly(await page.content());
  await audit(page, info);

  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await expect(page).toHaveURL(`${fixture.admin}/login`);
  expect(await hasSession(page)).toBe(false);
  const denied = await page.request.get(`${fixture.admin}/api/v1/t/${fixture.ownerTenantId}/products`, { maxRedirects: 0 });
  expect(denied.status()).toBe(401);
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
