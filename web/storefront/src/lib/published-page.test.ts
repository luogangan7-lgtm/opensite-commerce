/** Responsibility: request-scoped SSR response regressions; dependency: public HTTP client.
 * Caller: Bun frontend gate; invariants: HTTP status/cache precede rendering and one upstream read per view.
 */
import { describe, expect, test } from 'bun:test';
import type { PublishedSite } from '@opensite/api-client';
import { preparePublishedPage } from './published-page';
import { readAllowedHosts, requestCanonical } from './host-policy';

// Unbundled tests read the deployment allowlist per call; these are the hosts used below.
const testHosts = '127.0.0.1,localhost,shop.example.test,other.example.test';
process.env.OSC_STOREFRONT_ALLOWED_HOSTS = testHosts;

function requestContext(url: URL, response: { status: number; statusText: string; headers: Headers }, headers: Record<string, string> = {}) {
  return { url, response, request: new Request(url, { headers: { host: url.host, ...headers } }) };
}

const site: PublishedSite = {
  site_id: 'test-site', release_id: 'test-release', hostname: 'shop.example.test', locale: 'en',
  title: 'Test store', description: 'Published description', navigation: [],
  pages: [{ path: '/', title: 'Home', description: 'Home description', blocks: [] },
    { path: '/guides/setup', title: 'Setup', description: 'Setup description', blocks: [] }],
  products: [{ id: 'product', slug: 'tote', name: 'Tote', description: 'Tote description', unit_amount: '1299', currency: 'USD' }],
};

describe('published page prepares the route response before rendering', () => {
  test('double-slash pathname cannot replace canonical authority or expose URL credentials', () => {
    for (const requested of [
      'https://shop.example.test//external.example/path?token=private#section',
      'https://mock-user:mock-password@shop.example.test//external.example/path?token=private#section',
    ]) {
      const url = new URL(requested);
      const request = new Request(url.origin, { headers: { host: url.host } });
      const canonical = requestCanonical({ url, request });
      expect(canonical).toBe('https://shop.example.test//external.example/path');
      const parsed = new URL(canonical!);
      expect(parsed.origin).toBe('https://shop.example.test');
      expect(parsed.pathname).toBe('//external.example/path');
      expect(parsed.username).toBe('');
      expect(parsed.password).toBe('');
      expect(parsed.search).toBe('');
      expect(parsed.hash).toBe('');
    }
  });
  test('canonical stays with each top-level request origin/path and excludes query/fragment', async () => {
    const upstream = Bun.serve({ port: 0, fetch() { return Response.json(site); } });
    try {
      for (const [requested, path, expected] of [
        ['http://127.0.0.1:4321/guides/setup?campaign=one#section', '/guides/setup', 'http://127.0.0.1:4321/guides/setup'],
        ['https://other.example.test/products/tote?campaign=two#details', '/products/tote', 'https://other.example.test/products/tote'],
        ['https://other.example.test/missing?campaign=three#details', '/missing', 'https://other.example.test/missing'],
      ] as const) {
        const response = { status: 200, statusText: 'OK', headers: new Headers() };
        const view = await preparePublishedPage(requestContext(new URL(requested), response), path, upstream.url.origin);
        expect(view.canonical).toBe(expected);
      }
    } finally { upstream.stop(true); }
  });
  test('all successful views reuse exactly one request and return page/product metadata', async () => {
    const requests: URL[] = [];
    const upstream = Bun.serve({ port: 0, fetch(request) {
      requests.push(new URL(request.url));
      return Response.json(site);
    } });
    try {
      for (const [path, title, description] of [
        ['/', 'Home', 'Home description'], ['/guides/setup', 'Setup', 'Setup description'],
        ['/products', 'Products', 'Published description'], ['/products/tote', 'Tote', 'Tote description'],
      ]) {
        const response = { status: 200, statusText: 'OK', headers: new Headers() };
        const view = await preparePublishedPage(requestContext(new URL(`https://SHOP.Example.test${path}`), response), path!, upstream.url.origin);
        expect(response.status).toBe(200);
        expect(response.headers.get('Cache-Control')).toBe('no-store');
        expect(view.status).toBeUndefined();
        expect(view.title).toBe(title);
        expect(view.description).toBe(description);
      }
      expect(requests).toHaveLength(4);
      expect(requests.every(url => url.pathname === '/api/v1/public/sites/current' && url.searchParams.get('host') === 'shop.example.test')).toBe(true);
    } finally { upstream.stop(true); }
  });
  test('missing CMS/product and upstream failures preserve real status and no-store', async () => {
    let upstreamStatus = 200;
    const upstream = Bun.serve({ port: 0, fetch() { return Response.json(site, { status: upstreamStatus }); } });
    try {
      for (const [path, apiStatus, expected] of [
        ['/missing', 200, 404], ['/products/missing', 200, 404], ['/', 404, 404], ['/', 503, 502],
      ] as const) {
        upstreamStatus = apiStatus;
        const response = { status: 200, statusText: 'OK', headers: new Headers() };
        const view = await preparePublishedPage(requestContext(new URL(`https://shop.example.test${path}`), response), path, upstream.url.origin);
        expect(view.status).toBe(expected);
        expect(response.status).toBe(expected);
        expect(response.headers.get('Cache-Control')).toBe('no-store');
      }
    } finally { upstream.stop(true); }
  });
  test('invalid server configuration fails closed before rendering', async () => {
    const response = { status: 200, statusText: 'OK', headers: new Headers() };
    const view = await preparePublishedPage(requestContext(new URL('https://shop.example.test/'), response), '/', 'file:///private');
    expect(view.status).toBe(502);
    expect(response.status).toBe(502);
    expect(response.headers.get('Cache-Control')).toBe('no-store');
  });
  test('adapter fallback and forwarded host cannot select another tenant', async () => {
    let requests = 0;
    const upstream = Bun.serve({ port: 0, fetch() { requests++; return Response.json(site); } });
    try {
      const spoofed: Record<string, string>[] = [{ host: 'unknown.example.test:4321' }, { 'x-forwarded-host': 'localhost:4321' },
        { 'x-forwarded-proto': 'https' }, { 'x-forwarded-port': '443' }, { forwarded: 'host=localhost;proto=https' }];
      for (const headers of spoofed) {
        const response = { status: 200, statusText: 'OK', headers: new Headers() };
        const view = await preparePublishedPage(requestContext(new URL('http://localhost:4321/'), response, headers), '/', upstream.url.origin);
        expect(view.status).toBe(404);
        expect(response.status).toBe(404);
        expect(view.canonical).toBeUndefined();
      }
      expect(requests).toBe(0);
    } finally { upstream.stop(true); }
  });
  test('configured host allowlist is explicit and contains no wildcard or URL authority', () => {
    expect(readAllowedHosts(undefined)).toEqual(['127.0.0.1', 'localhost']);
    expect(readAllowedHosts('SHOP.Example.test.,other.example.test')).toEqual(['shop.example.test', 'other.example.test']);
    for (const value of ['', '*', '*.example.test', 'https://shop.example.test', 'shop.example.test:443', 'user@shop.example.test']) {
      expect(() => readAllowedHosts(value)).toThrow();
    }
  });
  test('Host outside the build allowlist is rejected even when it equals the adapter fallback URL', async () => {
    // Review thread rnC44: adapter fallback http://localhost:<port>/ must not admit localhost.
    let requests = 0;
    const upstream = Bun.serve({ port: 0, fetch() { requests++; return Response.json(site); } });
    process.env.OSC_STOREFRONT_ALLOWED_HOSTS = 'shop.example.test';
    try {
      for (const port of ['4321', '8080']) {
        const response = { status: 200, statusText: 'OK', headers: new Headers() };
        const view = await preparePublishedPage(requestContext(new URL(`http://localhost:${port}/`), response), '/', upstream.url.origin);
        expect(view.status).toBe(404);
        expect(response.status).toBe(404);
        expect(view.canonical).toBeUndefined();
      }
      expect(requests).toBe(0);
      expect(requestCanonical({ url: new URL('https://shop.example.test/a'), request: new Request('https://shop.example.test/a', { headers: { host: 'shop.example.test' } }) }))
        .toBe('https://shop.example.test/a');
    } finally { process.env.OSC_STOREFRONT_ALLOWED_HOSTS = testHosts; upstream.stop(true); }
  });
  test('Intl-invalid published locale becomes a controlled 502 before rendering', async () => {
    // Review threads rnC48/rnFif: formatting must not throw after the response status is chosen.
    let locale = 'en-US-US';
    const upstream = Bun.serve({ port: 0, fetch() { return Response.json({ ...site, locale }); } });
    try {
      for (const [value, path, expected] of [['en-US-US', '/products', 502], ['en-abcd-abcd', '/products/tote', 502],
        ['en-US', '/products', 200], ['zh-Hant-TW', '/products/tote', 200]] as const) {
        locale = value;
        const response = { status: 200, statusText: 'OK', headers: new Headers() };
        const view = await preparePublishedPage(requestContext(new URL(`https://shop.example.test${path}`), response), path, upstream.url.origin);
        expect(response.status).toBe(expected);
        expect(view.status).toBe(expected === 200 ? undefined : expected);
        expect(response.headers.get('Cache-Control')).toBe('no-store');
      }
    } finally { upstream.stop(true); }
  });
});
