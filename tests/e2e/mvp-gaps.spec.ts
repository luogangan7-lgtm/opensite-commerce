/** Responsibility: expose whole MVP browser journeys that current main cannot execute.
 * Dependencies: real pending feature PRs; caller: CI E2E and acceptance matrix.
 * Invariants: FIXME is never PASS; removing it requires a real scenario, not an empty body.
 */
import { expect, test } from '@playwright/test';
import { audit, fixture } from './fixture';

const staffSite = 'c3300003-0000-4000-8000-000000000001';
const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg==', 'base64');

test('real staff create SKU → media upload → publish', async ({ page }, info) => {
  test.setTimeout(240_000);
  const tenant = fixture.ownerTenantId, base = `${fixture.admin}/api/v1/t/${tenant}`;
  const slug = `staff-${info.project.name}`, title = `Staff product ${info.project.name}`;
  await page.goto(`${fixture.admin}/t/${tenant}/products/new`);
  await page.getByLabel('Email', { exact: true }).fill(fixture.ownerEmail);
  await page.getByLabel('Password', { exact: true }).fill(fixture.ownerPassword);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await page.getByLabel('Product code', { exact: true }).fill(slug);
  await page.getByLabel('Lifecycle', { exact: true }).selectOption('ready');
  const creation = page.waitForResponse(r => r.url() === `${base}/products` && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Create product', exact: true }).click();
  const product = await (await creation).json();
  await expect(page).toHaveURL(`${fixture.admin}/t/${tenant}/products/${product.id}`);
  await page.getByLabel('SKU code', { exact: true }).fill(slug);
  await page.getByLabel('Lifecycle', { exact: true }).nth(1).selectOption('ready');
  const skuCreation = page.waitForResponse(r => r.url() === `${base}/products/${product.id}/variants` && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Create SKU', exact: true }).click();
  const sku = await (await skuCreation).json();
  await page.getByRole('button', { name: `Edit ${slug.toUpperCase()}`, exact: true }).click();
  const session = await (await page.request.get(`${fixture.admin}/api/v1/auth/session`)).json();
  const headers = { Origin: fixture.admin, 'X-CSRF-Token': session.csrf_token, 'Content-Type': 'application/json' };
  const write = (path: string, data: unknown, etag?: string) => page.request.put(base + path, {
    data, headers: { ...headers, 'Idempotency-Key': crypto.randomUUID(), ...(etag ? { 'If-Match': etag } : {}) },
  });
  const listResponse = await page.request.post(`${base}/price-lists`, { headers, data: {
    currency: 'USD', status: 'active', starts_at: '2020-01-01T00:00:00Z', priority: 0, audience: 'all',
  } });
  expect(listResponse.status()).toBe(201);
  const list = await listResponse.json();
  await page.getByRole('button', { name: 'Reload price lists', exact: true }).click();
  await page.getByLabel('Price list', { exact: true }).selectOption(list.id);
  await page.getByLabel('Price in minor units', { exact: true }).fill('3456');
  const priceWrite = page.waitForResponse(r => r.url() === `${base}/price-lists/${list.id}/variants/${sku.id}/prices/1` && r.request().method() === 'PUT');
  await page.getByRole('button', { name: 'Save price', exact: true }).click();
  const savedPrice = await priceWrite;
  expect(savedPrice.status()).toBe(200);
  expect(savedPrice.request().postDataJSON()).toEqual({ unit_amount: 3456 });
  await expect(page.getByRole('status').filter({ hasText: 'Price saved' })).toBeVisible();
  // Native/JS validation and the independent real API both reject invalid money.
  for (const invalid of ['-1', '9223372036854775808']) {
    await page.getByLabel('Price in minor units', { exact: true }).fill(invalid);
    await page.getByRole('button', { name: 'Save price', exact: true }).click();
    expect(await page.getByLabel('Price in minor units', { exact: true }).evaluate(e => (e as HTMLInputElement).validity.valid)).toBe(false);
    const current = await (await page.request.get(`${base}/price-lists/${list.id}`)).json();
    const response = await write(`/price-lists/${list.id}/variants/${sku.id}/prices/1`, `{"unit_amount":${invalid}}`, `"${current.version}"`);
    expect([400, 422]).toContain(response.status());
  }
  await page.getByLabel('Price in minor units', { exact: true }).fill('3456');
  // Force a genuine stale price-list version; the editor must retain the draft.
  const current = await (await page.request.get(`${base}/price-lists/${list.id}`)).json();
  expect((await write(`/price-lists/${list.id}/variants/${sku.id}/prices/1`, { unit_amount: 7890 }, `"${current.version}"`)).status()).toBe(200);
  await page.getByRole('button', { name: 'Save price', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Save price', exact: true })).toBeDisabled();
  await expect(page.getByLabel('Price in minor units', { exact: true })).toHaveValue('3456');
  await page.getByRole('button', { name: 'Reload price from server', exact: true }).click();
  await page.getByLabel('Price in minor units', { exact: true }).fill('3456');
  await page.getByRole('button', { name: 'Save price', exact: true }).click();
  await expect(page.getByRole('status').filter({ hasText: 'Price saved' })).toBeVisible();
  await page.getByLabel('Media file').setInputFiles({ name: 'staff.png', mimeType: 'image/png', buffer: png });
  await page.getByRole('button', { name: 'Upload media', exact: true }).click();
  await expect(page.getByLabel('Media ID', { exact: true })).not.toHaveValue('');
  await page.getByRole('button', { name: 'Attach media', exact: true }).click();
  await expect(page.getByText('Media attached to this product.', { exact: true })).toBeVisible();
  await page.getByLabel('Locale', { exact: true }).fill('en');
  await page.getByRole('button', { name: 'Load content', exact: true }).click();
  await page.getByLabel('Title', { exact: true }).fill(title);
  await page.getByLabel('Body blocks JSON', { exact: true }).fill('[{"kind":"paragraph","text":"Staff body"}]');
  await page.getByLabel('SEO JSON', { exact: true }).fill('{"description":"Staff description"}');
  await page.getByRole('button', { name: 'Save content', exact: true }).click();
  await expect(page.getByText('Content saved.', { exact: true })).toBeVisible();
  await page.getByLabel('Site ID', { exact: true }).fill(staffSite);
  await page.getByRole('button', { name: 'Load listing', exact: true }).click();
  await page.getByLabel('Slug', { exact: true }).fill(slug);
  await page.getByLabel('Listing state', { exact: true }).selectOption('listed');
  await page.getByRole('button', { name: 'Save listing', exact: true }).click();
  await expect(page.getByText('Listing saved.', { exact: true })).toBeVisible();
  await page.getByLabel('Title', { exact: true }).fill(title + ' edited');
  await page.getByRole('button', { name: 'Save content', exact: true }).click();
  await expect(page.getByText('Content saved.', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Load listing', exact: true }).click();
  await page.getByRole('button', { name: 'Save listing', exact: true }).click();
  await expect(page.getByText('Listing saved.', { exact: true })).toBeVisible();
  await audit(page, info);
  // CMS writes use the same authenticated staff session, never a fabricated key.
  const drafts = `${base}/sites/${staffSite}/drafts`;
  const draftResponse = await page.request.post(drafts, { headers: { ...headers, 'Idempotency-Key': crypto.randomUUID() },
    data: { locale: 'en', title: 'Staff shop', description: 'Staff publication' } });
  expect(draftResponse.status()).toBe(201);
  const draft = await draftResponse.json();
  const published = await page.request.post(`${drafts}/${draft.draft_id}/publish`, {
    headers: { ...headers, 'Idempotency-Key': crypto.randomUUID(), 'If-Match': draftResponse.headers().etag },
    data: { expected_release_id: draft.base_release_id, expected_publication_revision: draft.publication_revision, renderer_build_id: 'admin-pricing-3' },
  });
  expect(published.status()).toBe(201);
  const release = await published.json();
  await expect.poll(async () => {
    const response = await page.request.get(`${fixture.api}/api/v1/public/sites/current?host=localhost`);
    return response.status() === 200 ? (await response.json()).products.find((p: { slug: string }) => p.slug === slug) : null;
  }).toMatchObject({ name: title + ' edited', skus: expect.arrayContaining([
    expect.objectContaining({ sku_id: sku.id, sku_code: slug.toUpperCase(), price_minor: '3456', currency: 'USD' }),
  ]) });
  // Publication alone cannot prove serving invalidation: change an already served listing.
  await page.getByRole('button', { name: 'Load listing', exact: true }).click();
  await page.getByLabel('Listing state', { exact: true }).selectOption('unlisted');
  await page.getByRole('button', { name: 'Save listing', exact: true }).click();
  await expect(page.getByText('Listing saved.', { exact: true })).toBeVisible();
  await expect.poll(async () => {
    const response = await page.request.get(`${fixture.api}/api/v1/public/sites/current?host=localhost`);
    return response.status() === 200 ? (await response.json()).products.some((p: { slug: string }) => p.slug === slug) : true;
  }, { message: 'Listing invalidation must remove the product from the served release', timeout: 20_000 }).toBe(false);
  await page.getByLabel('Listing state', { exact: true }).selectOption('listed');
  await page.getByRole('button', { name: 'Save listing', exact: true }).click();
  await expect(page.getByText('Listing saved.', { exact: true })).toBeVisible();
  await expect.poll(async () => {
    const response = await page.request.get(`${fixture.api}/api/v1/public/sites/current?host=localhost`);
    return response.status() === 200 ? (await response.json()).products.some((p: { slug: string }) => p.slug === slug) : false;
  }).toBe(true);
  await page.goto(`http://localhost:4322/products/${slug}`);
  await expect(page.getByRole('heading', { name: title + ' edited', exact: true })).toBeVisible();
  await expect(page.getByText('$34.56', { exact: false })).toBeVisible();
  await expect(page.getByText('Staff description', { exact: true })).toBeVisible();
  await expect(page.getByRole('option', { name: new RegExp(slug.toUpperCase()) })).toHaveCount(1);
  await audit(page, info);
  expect((await page.request.get(`${fixture.admin}/api/v1/t/${fixture.tenantId}/products/${product.id}`)).status()).toBe(404);
  for (const path of [`/products/${product.id}/content/en`, `/products/${product.id}/listings/${staffSite}/en`, `/price-lists/${list.id}/variants/${sku.id}/prices/1`]) {
    expect((await page.request.put(`${fixture.admin}/api/v1/t/${fixture.tenantId}${path}`, { headers: { ...headers, 'If-Match': '"0"', 'Idempotency-Key': crypto.randomUUID() }, data: {} })).status()).toBe(404);
  }
  await info.attach('staff-publication', { body: JSON.stringify({ product: product.id, sku: sku.id, release: release.release_id }), contentType: 'application/json' });
  for (const role of ['viewer', 'content_editor']) {
    await page.goto(`${fixture.admin}/`);
    await page.getByRole('button', { name: 'Sign out', exact: true }).click();
    await page.getByLabel('Email', { exact: true }).fill(`staff-${role}@example.test`);
    await page.getByLabel('Password', { exact: true }).fill(fixture.ownerPassword);
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await page.goto(`${fixture.admin}/t/${tenant}/products/${product.id}`);
    await expect(page.getByText('Catalog editing is unavailable for this session.', { exact: true })).toBeVisible();
    for (const name of ['Save price', 'Save content', 'Save listing']) await expect(page.getByRole('button', { name, exact: true })).toHaveCount(0);
    const restricted = await (await page.request.get(`${fixture.admin}/api/v1/auth/session`)).json();
    for (const path of [`/price-lists/${list.id}/variants/${sku.id}/prices/1`, `/products/${product.id}/content/en`, `/products/${product.id}/listings/${staffSite}/en`]) {
      const denied = await page.request.put(base + path, { headers: { Origin: fixture.admin, 'X-CSRF-Token': restricted.csrf_token, 'If-Match': '"0"', 'Idempotency-Key': crypto.randomUUID() }, data: {} });
      expect(denied.status()).toBe(403);
    }
  }
});

test('real admin orders list and detail use persisted orders and staff scopes', async ({ page }, info) => {
  const suffix = info.project.name === 'desktop' ? '000000000001' : '000000000002';
  const tenant = `c1900010-0000-4000-8000-${suffix}`;
  const orderId = `c1900003-0000-4000-8000-${suffix}`;
  const base = `${fixture.admin}/api/v1/t/${tenant}/orders`;
  // The production route guard must deny a direct private orders URL before reads.
  await page.goto(`${fixture.admin}/t/${tenant}/orders`);
  await expect(page).toHaveURL(`${fixture.admin}/login`);
  await page.getByLabel('Email', { exact: true }).fill(fixture.ownerEmail);
  await page.getByLabel('Password', { exact: true }).fill(fixture.ownerPassword);
  const listed = page.waitForResponse(r => r.url().startsWith(base + '?') && r.request().method() === 'GET');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('table', { name: 'Order list' })).toBeVisible();
  const response = await listed;
  expect(response.status()).toBe(200);
  const list = await response.json();
  expect(list.items).toEqual([expect.objectContaining({ id: orderId, state: 'pending_payment',
    total_minor: '270', currency: 'USD', customer_kind: 'guest' })]);
  await expect(page.getByText('CONTRACT FIXTURE')).toHaveCount(0);
  await audit(page, info);
  const detailRead = page.waitForResponse(r => r.url() === `${base}/${orderId}`);
  await page.getByRole('link', { name: String(list.items[0].order_number), exact: true }).click();
  const detail = await detailRead;
  expect(detail.status()).toBe(200);
  const order = await detail.json();
  expect(detail.headers().etag).toBe(`"${order.version}"`);
  expect(order).toMatchObject({ id: orderId, state: 'pending_payment', total_minor: '270', currency: 'USD',
    lines: [expect.objectContaining({ sku_code: 'C19-WEBHOOK', quantity: 2, line_total_minor: '220' })] });
  expect(order).not.toHaveProperty('guest_contact');
  await expect(page.getByRole('heading', { name: `Order ${order.order_number}`, exact: true })).toBeVisible();
  await expect(page.getByRole('cell', { name: 'C19-WEBHOOK', exact: true })).toBeVisible();
  await expect(page.getByText('USD 2.70', { exact: true })).toBeVisible();
  await audit(page, info);
  // Membership in this tenant cannot expose an order through a different tenant path.
  expect((await page.request.get(`${fixture.admin}/api/v1/t/${fixture.ownerTenantId}/orders/${orderId}`)).status()).toBe(404);
  expect((await page.request.get(`${fixture.admin}/api/v1/t/${fixture.tenantId}/orders`)).status()).toBe(404);
  const loggedOut = page.waitForResponse(r => r.url() === `${fixture.admin}/api/v1/auth/logout` && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  expect((await loggedOut).status()).toBe(204);
  await expect(page).toHaveURL(`${fixture.admin}/login`);
  expect((await page.request.get(base)).status()).toBe(401);
  await info.attach('admin-order-read', { body: JSON.stringify({ tenant, list, order }), contentType: 'application/json' });
});

