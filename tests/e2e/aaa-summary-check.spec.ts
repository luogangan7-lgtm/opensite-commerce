/** Temporary failure-path exercise: public output must omit response/IO values. */
import { createServer } from 'node:http';
import { randomUUID } from 'node:crypto';
import { expect } from '@playwright/test';
import { test } from './fixture';

test('summary failure-path exercise', async ({ request }, info) => {
  const payload = 'PRIVATE_RESPONSE_CANARY_374_' + randomUUID();
  const server = createServer((_request, response) => {
    response.writeHead(200, { 'Content-Type': 'text/plain' });
    response.end(payload);
  });
  await new Promise<void>(done => { server.listen(0, '127.0.0.1', done); });
  try {
    const address = server.address();
    if (!address || typeof address === 'string') throw new Error('Probe listener unavailable');
    const response = await request.get(`http://127.0.0.1:${address.port}/`);
    console.log(payload);
    console.error(payload);
    await info.attach('response-body', { body: Buffer.from(payload), contentType: 'text/plain' });
    expect(await response.text(), 'Failure-path response assertion').toBe('expected response');
  } finally {
    await new Promise<void>((done, reject) => { server.close(error => error ? reject(error) : done()); });
  }
});
