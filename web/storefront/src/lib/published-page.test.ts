/** Responsibility: request-scoped SSR response regressions; dependency: public HTTP client.
 * Caller: Bun frontend gate; invariants: HTTP status/cache precede rendering and one upstream read per view.
 */
import { describe, expect, test } from 'bun:test';
import type { PublishedSite } from '@opensite/api-client';
import { preparePublishedPage } from './published-page';

const site: PublishedSite = {
  site_id: 'test-site', release_id: 'test-release', hostname: 'shop.example.test', locale: 'en',
  title: 'Test store', description: 'Published description', navigation: [],
  pages: [{ path: '/', title: 'Home', description: 'Home description', blocks: [] },
    { path: '/guides/setup', title: 'Setup', description: 'Setup description', blocks: [] }],
  products: [{ id: 'product', slug: 'tote', name: 'Tote', description: 'Tote description', unit_amount: '1299', currency: 'USD' }],
};

describe('published page prepares the route response before rendering', () => {
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
        const view = await preparePublishedPage({ url: new URL(`https://SHOP.Example.test${path}`), response }, path!, upstream.url.origin);
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
        const view = await preparePublishedPage({ url: new URL(`https://shop.example.test${path}`), response }, path, upstream.url.origin);
        expect(view.status).toBe(expected);
        expect(response.status).toBe(expected);
        expect(response.headers.get('Cache-Control')).toBe('no-store');
      }
    } finally { upstream.stop(true); }
  });
  test('invalid server configuration fails closed before rendering', async () => {
    const response = { status: 200, statusText: 'OK', headers: new Headers() };
    const view = await preparePublishedPage({ url: new URL('https://shop.example.test/'), response }, '/', 'file:///private');
    expect(view.status).toBe(502);
    expect(response.status).toBe(502);
    expect(response.headers.get('Cache-Control')).toBe('no-store');
  });
});
