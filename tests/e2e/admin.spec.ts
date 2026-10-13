/** Responsibility: exercise real admin forms and deny unauthenticated private operations.
 * Dependencies: production admin static build and same-origin proxy to actual API; caller: CI E2E.
 * Invariants: no simulated identity, cookie fabrication or API interception; absent login stays explicit.
 */
import { expect, test } from '@playwright/test';
import { assertPublicOnly, audit, fixture } from './fixture';

test('real admin login form reports unavailable authentication safely', async ({ page }, info) => {
  const session = page.waitForResponse(response =>
    response.url() === `${fixture.admin}/api/v1/auth/session` && response.request().method() === 'GET');
  await page.goto(`${fixture.admin}/login`);
  // Main lacks PR43's session operation. Require its actual API problem response;
  // a static-server SPA fallback returning HTML 200 must fail this harness.
  const sessionResponse = await session;
  expect(sessionResponse.status()).toBe(404);
  expect(sessionResponse.headers()['content-type']).toContain('application/problem+json');
  expect(await sessionResponse.json()).toMatchObject({ status: 404, code: 'not_found' });
  await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
  await expect(page.getByLabel('Email', { exact: true })).toBeVisible();
  await expect(page.getByLabel('Password', { exact: true })).toBeVisible();
  await audit(page, info);

  // Native form validation rejects a malformed email before an auth attempt.
  let loginAttempts = 0;
  page.on('request', request => { if (request.url() === `${fixture.admin}/api/v1/auth/login`) loginAttempts++; });
  await page.getByLabel('Email', { exact: true }).fill('invalid-email');
  await page.getByLabel('Password', { exact: true }).fill(crypto.randomUUID());
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  expect(await page.getByLabel('Email', { exact: true }).evaluate(element =>
    (element as HTMLInputElement).validity.typeMismatch)).toBe(true);
  expect(loginAttempts).toBe(0);
  await audit(page, info);

  await page.getByLabel('Email', { exact: true }).fill('c24-nobody@example.invalid');
  const challenge = page.waitForResponse(response =>
    response.url() === `${fixture.admin}/api/v1/auth/login` && response.request().method() === 'GET');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  const challengeResponse = await challenge;
  expect(challengeResponse.status()).toBe(404);
  expect(challengeResponse.headers()['content-type']).toContain('application/problem+json');
  expect(await challengeResponse.json()).toMatchObject({ status: 404, code: 'not_found' });
  await expect(page.getByRole('alert')).toHaveText('Unable to sign in. Check your credentials or try again later.');
  await expect(page.getByRole('alert')).toBeFocused();
  await expect(page.getByLabel('Password', { exact: true })).toHaveValue('');
  expect(loginAttempts).toBe(1);
  expect((await page.context().cookies()).some(cookie => cookie.name === '__Host-osc_admin_session')).toBe(false);
  assertPublicOnly(await page.content());
  await audit(page, info);
});

test('real guarded catalog route shows honest session-unavailable state without private reads', async ({ page }, info) => {
  const privateReads: string[] = [];
  page.on('request', request => {
    if (new URL(request.url()).pathname.startsWith(`/api/v1/t/${fixture.tenantId}/`)) privateReads.push(request.url());
  });
  const session = page.waitForResponse(response => response.url() === `${fixture.admin}/api/v1/auth/session`);
  await page.goto(`${fixture.admin}/t/${fixture.tenantId}/products`);
  expect((await session).status()).toBe(404);
  await expect(page.getByRole('alert')).toHaveText('Unable to check your session. Try again.');
  await expect(page.getByRole('button', { name: 'Retry session' })).toBeEnabled();
  expect(privateReads).toEqual([]);
  await expect(page.getByRole('link', { name: 'Create product', exact: true })).toHaveCount(0);
  await expect(page.getByText(fixture.productName, { exact: true })).toHaveCount(0);
  await audit(page, info);
  const retry = page.waitForResponse(response => response.url() === `${fixture.admin}/api/v1/auth/session`);
  await page.getByRole('button', { name: 'Retry session' }).click();
  expect((await retry).status()).toBe(404);
  await expect(page.getByRole('alert')).toHaveText('Unable to check your session. Try again.');
  expect(privateReads).toEqual([]);
  assertPublicOnly(await page.content());
  await audit(page, info);
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
