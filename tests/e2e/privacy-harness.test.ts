/** Offline regression for the real APIRequestContext wrapper (no browser/container).
 * CI-only check_all invokes Node; assertion failures never print observed values.
 */
import { test, expect } from '@playwright/test';
import { createServer } from 'node:http';
import { randomBytes } from 'node:crypto';
import { request as factory, type Browser } from '@playwright/test';
import { installPrivacy } from './privacy-harness';

test('automatic API observation includes headers cookies nested bodies and generated credentials', async () => {
  const samples = Array.from({ length: 8 }, () => randomBytes(24).toString('hex'));
  const server = createServer((req, res) => {
    req.resume();
    res.writeHead(200, { 'content-type': 'application/json', 'set-cookie': [`session=${samples[0]}; HttpOnly`, `other=${samples[1]}`],
      'x-csrf-token': samples[2] });
    res.end(JSON.stringify({ nested: { csrf_token: samples[3], proof: samples[4] } }));
  });
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
  const context = await factory.newContext();
  const found = new Set<string>();
  const browser = { contexts: () => [], newContext: async () => { throw new Error('unused'); } } as unknown as Browser;
  const finish = installPrivacy(browser, context, { request: factory },
    async (...values) => { values.forEach(value => found.add(value)); });
  try {
    const port = (server.address() as { port: number }).port;
    await context.post(`http://127.0.0.1:${port}`, { headers: { Authorization: `Bearer ${samples[5]}` },
      data: { password: samples[6], nested: { secret_key: samples[7] } } });
    await finish();
    expect(samples.every(value => found.has(value))).toBe(true);
  } finally { await context.dispose(); server.close(); }
});

if (process.env.E2E_PRIVACY_BROWSER_SMOKE === 'true') test('automatic browser observation survives redirects and context teardown', async ({ browser }) => {
  const secret = randomBytes(24).toString('hex'), found = new Set<string>();
  const server = createServer((req, res) => {
    if (req.url === '/redirect') {
      res.writeHead(302, { location: '/', 'content-type': 'text/html', 'set-cookie': `session=${secret}` });
      res.end();
    } else {
      res.writeHead(200, { 'content-type': 'text/html' });
      res.end(`<input name="csrf_token" value="${secret}">`);
    }
  });
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
  const context = await factory.newContext();
  const finish = installPrivacy(browser, context, { request: factory }, async (...values) => {
    values.forEach(value => found.add(value));
  });
  try {
    const page = await browser.newPage();
    await page.goto(`http://127.0.0.1:${(server.address() as { port: number }).port}/redirect`);
    await page.context().close();
    await finish();
    expect(found.has(secret)).toBe(true);
  } finally { await context.dispose(); server.close(); }
});
