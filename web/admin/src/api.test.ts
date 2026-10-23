/** Transport contract gate; depends on Bun and public API; never contacts a real account. */
import { describe, expect, test } from 'bun:test';
import { AdminAPI, APIError, CatalogAPI } from './api';

const tenant = '11111111-1111-4111-8111-111111111111';
const product = '22222222-2222-4222-8222-222222222222';
const session = { user_id: tenant, session_id: product, expires_at: '2099-01-01T00:00:00Z', csrf_token: 'session-csrf' };
const json = (value: unknown, status = 200) => Response.json(value, { status });
function harness(responses: Response[]) {
  const calls: { url: string; init: RequestInit }[] = [];
  const fetcher = (async (url: string | URL | Request, init?: RequestInit) => {
    calls.push({ url: String(url), init: init ?? {} });
    const result = responses.shift(); if (!result) throw new Error('Unexpected request'); return result;
  }) as typeof fetch;
  return { calls, fetcher };
}
describe('cookie admin API', () => {
  for (const lateStatus of [200, 401]) test(`late bootstrap ${lateStatus} cannot overwrite or expire fresh login`, async () => {
    let release!: (response: Response) => void;
    const late = new Promise<Response>(resolve => { release = resolve; });
    let calls = 0, expired = 0, observed = '';
    const fresh = { ...session, csrf_token: 'fresh-csrf' };
    const api = new AdminAPI({ onSessionExpired: () => expired++, fetch: (async (_url, init) => {
      calls++;
      if (calls === 1) return late;
      if (calls === 2) return json({ csrf_token: 'challenge' });
      if (calls === 3 || calls === 4) return json(fresh);
      observed = new Headers(init?.headers).get('X-CSRF-Token') ?? ''; return json({ id: product });
    }) as typeof fetch });
    const bootstrap = api.session().catch(error => error);
    await api.login({ email: 'staff@example.test', password: 'fixture' });
    release(json(lateStatus === 200 ? { ...session, csrf_token: 'old-csrf' } : {}, lateStatus));
    const stale = await bootstrap;
    expect(expired).toBe(0);
    await new CatalogAPI(api, tenant).createProduct({ product_code: 'SAFE' });
    expect(observed).toBe('fresh-csrf');
    expect(stale).toMatchObject({ name: 'AbortError' });
  });
  test('late logout cannot wipe a newer successful login', async () => {
    let release!: (response: Response) => void;
    const late = new Promise<Response>(resolve => { release = resolve; });
    let calls = 0, observed = '';
    const api = new AdminAPI({ fetch: (async (_url, init) => {
      calls++;
      if (calls === 1) return json(session);
      if (calls === 2) return late;
      if (calls === 3) return json({ csrf_token: 'challenge' });
      if (calls === 4 || calls === 5) return json({ ...session, csrf_token: 'fresh-csrf' });
      observed = new Headers(init?.headers).get('X-CSRF-Token') ?? ''; return json({ id: product });
    }) as typeof fetch });
    await api.session(); const logout = api.logout().catch(error => error);
    await api.login({ email: 'staff@example.test', password: 'fixture' });
    release(new Response(null, { status: 204 })); const stale = await logout;
    await new CatalogAPI(api, tenant).createProduct({ product_code: 'SAFE' });
    expect(observed).toBe('fresh-csrf'); expect(stale).toMatchObject({ name: 'AbortError' });
  });
  test('aborted response cannot install CSRF or expire the active session', async () => {
    let release!: (response: Response) => void; const late = new Promise<Response>(resolve => { release = resolve; });
    let calls = 0, expired = 0;
    const api = new AdminAPI({ onSessionExpired: () => expired++, fetch: (async (_url: string | URL | Request) => ++calls === 1 ? json(session) : late) as typeof fetch });
    await api.session(); const controller = new AbortController(); const request = api.session(controller.signal).catch(error => error);
    controller.abort(); release(json({}, 401)); expect(await request).toMatchObject({ name: 'AbortError' }); expect(expired).toBe(0);
  });
  test('PR43 login challenge POST session lifecycle keeps session-bound CSRF and same-origin cookies', async () => {
    const h = harness([json({ csrf_token: 'challenge' }), json(session, 201), json(session), new Response(null, { status: 204 })]);
    const client = new AdminAPI({ fetch: h.fetcher });
    expect(await client.login({ email: 'staff@example.test', password: 'secret' })).toEqual(session);
    await client.logout();
    expect(h.calls.map(c => [c.url, c.init.method])).toEqual([
      ['/api/v1/auth/login', 'GET'], ['/api/v1/auth/login', 'POST'], ['/api/v1/auth/session', 'GET'], ['/api/v1/auth/logout', 'POST'],
    ]);
    for (const c of h.calls) {
      expect(c.init.credentials).toBe('same-origin');
      expect(c.init.redirect).toBe('error');
      expect(new Headers(c.init.headers).has('Authorization')).toBe(false);
    }
    expect(new Headers(h.calls[1].init.headers).get('X-CSRF-Token')).toBe('challenge');
    expect(new Headers(h.calls[3].init.headers).get('X-CSRF-Token')).toBe('session-csrf');
    expect(h.calls[1].init.body).toBe(JSON.stringify({ email: 'staff@example.test', password: 'secret' }));
  });
  test('401 expires once, clears CSRF, and never exposes server problem detail', async () => {
    let expired = 0;
    const h = harness([json(session), json({ code: 'secret-account-hint', detail: 'password=secret', title: 'private@example.test' }, 401), json({}, 401)]);
    const client = new AdminAPI({ fetch: h.fetcher, onSessionExpired: () => expired++ });
    await client.session(); const catalog = new CatalogAPI(client, tenant);
    let error: unknown; try { await catalog.listProducts(); } catch (caught) { error = caught; }
    expect(error).toBeInstanceOf(APIError); expect((error as APIError).status).toBe(401);
    expect(String(error)).not.toContain('secret'); expect((error as APIError).userHint).not.toContain('private');
    await expect(catalog.listProducts()).rejects.toBeInstanceOf(APIError); expect(expired).toBe(1);
    await expect(catalog.createProduct({ product_code: 'SAFE' })).rejects.toBeInstanceOf(APIError);
    expect(h.calls).toHaveLength(3);
  });
  test('malformed problem JSON uses status-only hint; stale/conflict statuses remain distinct', async () => {
    for (const status of [403, 409, 412, 422, 428, 429, 503]) {
      const h = harness([new Response('<secret>', { status, headers: { 'Content-Type': 'application/problem+json' } })]);
      const catalog = new CatalogAPI(new AdminAPI({ fetch: h.fetcher }), tenant);
      try { await catalog.listProducts(); throw new Error('expected error'); } catch (error) {
        expect(error).toBeInstanceOf(APIError); expect((error as APIError).status).toBe(status); expect(String(error)).not.toContain('secret');
      }
    }
  });
  test('login rejection is generic and does not invoke session expiry callback', async () => {
    let expired = 0; const h = harness([json({ csrf_token: 'challenge' }), json({ detail: 'unknown email' }, 401)]);
    const api = new AdminAPI({ fetch: h.fetcher, onSessionExpired: () => expired++ });
    await expect(api.login({ email: 'a@b.test', password: 'wrong' })).rejects.toMatchObject({ status: 401, userHint: 'Sign-in failed. Check your credentials and try again.' });
    expect(expired).toBe(0);
  });
  test('failed logout preserves CSRF for retry; only success or expiry clears it', async () => {
    const h = harness([json(session), json({}, 503), new Response(null, { status: 204 })]);
    const api = new AdminAPI({ fetch: h.fetcher }); await api.session();
    await expect(api.logout()).rejects.toMatchObject({ status: 503 });
    await api.logout(); expect(h.calls).toHaveLength(3);
    expect(new Headers(h.calls[2].init.headers).get('X-CSRF-Token')).toBe('session-csrf');
    await expect(api.logout()).rejects.toMatchObject({ status: 401 });
  });
  test('catalog PATCH and archives send quoted version, CSRF and idempotency; no status pseudo PATCH', async () => {
    const h = harness([json(session), json({ id: product, version: 2 }), new Response(null, { status: 204 })]);
    const api = new AdminAPI({ fetch: h.fetcher }); await api.session(); const catalog = new CatalogAPI(api, tenant);
    await catalog.updateProduct(product, { product_code: 'CODE' }, 1, { idempotencyKey: 'update-key' });
    await catalog.archiveProduct(product, 2, { idempotencyKey: 'archive-key' });
    expect(h.calls[1].url).toBe(`/api/v1/t/${tenant}/products/${product}`);
    expect(h.calls[1].init.method).toBe('PATCH'); expect(h.calls[2].init.method).toBe('DELETE');
    expect(new Headers(h.calls[1].init.headers).get('If-Match')).toBe('"1"');
    expect(new Headers(h.calls[1].init.headers).get('X-CSRF-Token')).toBe('session-csrf');
    expect(new Headers(h.calls[1].init.headers).get('Idempotency-Key')).toBe('update-key');
    await expect(catalog.updateProduct(product, { product_code: 'A' }, Number.MAX_SAFE_INTEGER + 1)).rejects.toThrow();
    expect(h.calls).toHaveLength(3);
  });
  test('catalog list cursor is query-encoded and invalid tenant IDs never reach fetch', async () => {
    const h = harness([json({ items: [] })]);
    await new CatalogAPI(new AdminAPI({ fetch: h.fetcher }), tenant).listProducts({ pageSize: 20, cursor: 'a+/=' });
    expect(h.calls[0].url).toBe(`/api/v1/t/${tenant}/products?page_size=20&cursor=a%2B%2F%3D`);
    expect(() => new CatalogAPI(new AdminAPI({ fetch: h.fetcher }), '../auth')).toThrow();
  });
  test('SKU CRUD follows exact variant routes and protects update/archive versions', async () => {
    const h = harness([json(session), json({ items: [] }), json({ id: product }), json({ id: product }, 201), json({ id: product }), new Response(null, { status: 204 })]);
    const api = new AdminAPI({ fetch: h.fetcher }); await api.session(); const catalog = new CatalogAPI(api, tenant);
    await catalog.listVariants(product); await catalog.getVariant(product, tenant);
    await catalog.createVariant(product, { sku: 'SKU', option_value_ids: [] });
    await catalog.updateVariant(product, tenant, { sku: 'SKU2' }, 3);
    await catalog.archiveVariant(product, tenant, 4);
    const base = `/api/v1/t/${tenant}/products/${product}/variants`;
    expect(h.calls.slice(1).map(c => [c.url, c.init.method])).toEqual([[base, 'GET'], [`${base}/${tenant}`, 'GET'], [base, 'POST'], [`${base}/${tenant}`, 'PATCH'], [`${base}/${tenant}`, 'DELETE']]);
    expect(new Headers(h.calls[4].init.headers).get('If-Match')).toBe('"3"');
    expect(new Headers(h.calls[5].init.headers).get('If-Match')).toBe('"4"');
  });
  test('problem JSON exposes only approved code plus local hint, never its title or detail', async () => {
    const h = harness([new Response(JSON.stringify({ code: 'version_conflict', detail: 'secret', title: '<script>secret</script>', status: 200 }), { status: 412, headers: { 'Content-Type': 'application/problem+json; charset=utf-8' } })]);
    await expect(new CatalogAPI(new AdminAPI({ fetch: h.fetcher }), tenant).listProducts()).rejects.toMatchObject({ status: 412, code: 'version_conflict', userHint: 'This resource changed. Reload it before saving.' });
  });
  test('raw response preserves representation headers without bypassing error mapping', async () => {
    const h = harness([new Response('{}', { headers: { ETag: '"4"' } }), json({}, 403)]);
    const api = new AdminAPI({ fetch: h.fetcher });
    const response = await api.request<Response>(`/api/v1/t/${tenant}/products/${product}`, 'GET', undefined, { responseRaw: true });
    expect(response.headers.get('ETag')).toBe('"4"');
    await expect(api.request<Response>(`/api/v1/t/${tenant}/products/${product}`, 'GET', undefined, { responseRaw: true })).rejects.toMatchObject({ status: 403 });
  });
  test('failed upload PUT never triggers complete or synthesized poll', async () => {
    const h = harness([json(session), json({ id: product, status: 'reserved', expires_at: session.expires_at }, 201), json({}, 422)]);
    const api = new AdminAPI({ fetch: h.fetcher }); await api.session();
    await expect(new CatalogAPI(api, tenant).uploadMedia(new File(['abc'], 'photo.png', { type: 'image/png' }))).rejects.toMatchObject({ status: 422 });
    expect(h.calls).toHaveLength(3);
  });
  test('upload hashes file and uses three supported same-origin operations only', async () => {
    const reserved = { id: product, status: 'reserved' as const, expires_at: session.expires_at };
    const stored = { ...reserved, status: 'stored' as const, media_id: product };
    const h = harness([json(session), json(reserved, 201), new Response(null, { status: 204 }), json(stored, 202)]);
    const api = new AdminAPI({ fetch: h.fetcher }); await api.session();
    const file = new File(['abc'], 'photo.png', { type: 'image/png' });
    expect(await new CatalogAPI(api, tenant).uploadMedia(file, { idempotencyKey: 'upload-key' })).toEqual(stored);
    expect(JSON.parse(String(h.calls[1].init.body))).toEqual({ filename: 'photo.png', size_bytes: 3, sha256: 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad', declared_mime: 'image/png' });
    expect(h.calls.map(c => c.init.method)).toEqual(['GET', 'POST', 'PUT', 'POST']);
    expect(h.calls[2].url).toBe(`/api/v1/t/${tenant}/media/uploads/${product}/content`);
    expect(new Headers(h.calls[2].init.headers).get('Content-Type')).toBe('application/octet-stream');
    expect(h.calls[2].init.body).toBe(file);
  });
});

describe('review follow-ups', () => {
  test('first-time bootstrap 401 is not reported as an expired session', async () => {
    let expired = 0;
    const api = new AdminAPI({ fetch: (async () => json({}, 401)) as unknown as typeof fetch, onSessionExpired: () => expired++ });
    await expect(api.session()).rejects.toMatchObject({ status: 401 }); expect(expired).toBe(0);
  });
  test('retrying one logical upload replays the same per-step idempotency keys; blank MIME omits declared_mime', async () => {
    const reserved = { id: product, status: 'reserved' as const, expires_at: session.expires_at };
    const stored = { ...reserved, status: 'stored' as const, media_id: product };
    const h = harness([json(session), json(reserved, 201), new Response(null, { status: 204 }), json({}, 503), json(reserved, 201), new Response(null, { status: 204 }), json(stored, 202)]);
    const api = new AdminAPI({ fetch: h.fetcher }); await api.session();
    const catalog = new CatalogAPI(api, tenant), file = new File(['abc'], 'photo.avif', { type: '' });
    await expect(catalog.uploadMedia(file, { idempotencyKey: 'k1' })).rejects.toMatchObject({ status: 503 });
    expect(await catalog.uploadMedia(file, { idempotencyKey: 'k1' })).toEqual(stored);
    const keys = h.calls.slice(1).map(c => new Headers(c.init.headers).get('Idempotency-Key'));
    expect(keys).toEqual(['k1.reserve', 'k1.content', 'k1.complete', 'k1.reserve', 'k1.content', 'k1.complete']);
    expect(JSON.parse(String(h.calls[1].init.body))).not.toHaveProperty('declared_mime');
  });
  test('uppercase UUIDs are canonicalized to lowercase paths', async () => {
    const h = harness([json({ items: [] })]);
    await new CatalogAPI(new AdminAPI({ fetch: h.fetcher }), tenant.toUpperCase().replace('4', 'A')).listProducts();
    expect(h.calls[0].url).toBe(`/api/v1/t/${tenant.replace('4', 'a')}/products`);
  });
});

 test('generated staff client echoes opaque ETag and sends full int64 minor units exactly', async () => {
   const tag = '"listing-v9-content-v7"';
   const h = harness([json(session), Response.json({ site_id: tenant, locale: 'en', slug: 'sample', listed: true, content_revision: 7, revision: 7 }, { headers: { ETag: tag } }),
     Response.json({ site_id: tenant, locale: 'en', slug: 'sample', listed: false, content_revision: 7, revision: 7 }, { headers: { ETag: '"new-tag"' } }), json({ id: tenant, version: 3 })]);
   const transport = new AdminAPI({ fetch: h.fetcher }); await transport.session();
   const api = new CatalogAPI(transport, tenant), listing = await api.listing(product, tenant, 'en');
   await api.saveListing(product, tenant, 'en', listing.etag, { slug: 'sample', listed: false, content_revision: 7 });
   expect(new Headers(h.calls[2].init.headers).get('If-Match')).toBe(tag);
   await api.savePrice({ id: tenant, version: 2, currency: 'USD', status: 'active', starts_at: '2020-01-01T00:00:00Z', priority: 0, audience: 'all' }, product, '9223372036854775807');
   expect(h.calls[3].init.body).toBe('{"unit_amount":9223372036854775807}');
   for (const invalid of ['-1', '1.5', '1e3', '9223372036854775808']) {
     await expect(api.savePrice({ id: tenant, version: 2 } as never, product, invalid)).rejects.toMatchObject({ status: 422 });
   }
   expect(h.calls).toHaveLength(4);
 });
