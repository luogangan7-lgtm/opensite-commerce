/** First-paint routes must not download the contact-only product catalog. */
import { test, expect } from '@playwright/test';
for (const route of ['/', '/products/hoodie-set-k02/']) test(`no contact catalog on ${route}`, async ({ page }) => {
 const scripts: {url: string; bytes: number}[] = [];
 const pending: Promise<void>[] = [];
 page.on('response', response => {
  if (response.request().resourceType() === 'script') pending.push(response.body().then(body => { scripts.push({url: response.url(), bytes: body.length}); }));
 });
 await page.goto(route, {waitUntil: 'networkidle'});
 await Promise.all(pending);
 expect(scripts.filter(s => s.bytes > 100000)).toEqual([]);
});
