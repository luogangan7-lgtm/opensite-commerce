/** Synthetic browser → actual Pages handler → stub storage; no real storage or buyer data. */
import { test, expect } from '@playwright/test';
import { onRequestPost } from '../functions/api/rfq';
const id = '524f5000-0000-4000-8000-000000000001';
const env = { SUPABASE_URL: 'https://database.example.invalid', SUPABASE_SECRET_KEY: 'ci-private-placeholder' };
for (const mode of ['success', 'validation', 'timeout', 'duplicate'] as const) {
  test(`RFQ chain ${mode}`, async ({ page }) => {
    let writes = 0, calls = 0;
    const logs: string[] = [];
    page.on('console', msg => logs.push(msg.text()));
    await page.route('**/api/rfq', async route => {
      calls++;
      const original = globalThis.fetch;
      globalThis.fetch = async (_input, init) => {
        writes++;
        expect(init?.redirect).toBe('manual');
        expect(JSON.parse(String(init?.body)).email).toBe('ci@example.invalid');
        if (mode === 'timeout') throw new DOMException('synthetic timeout', 'TimeoutError');
        return Response.json([{ id }], { status: 201 });
      };
      try {
        const body = JSON.parse(route.request().postData()!);
        if (mode === 'validation') body.email = 'invalid';
        const response = await onRequestPost({ request: new Request('https://site.example.invalid/api/rfq', { method: 'POST', body: JSON.stringify(body) }), env });
        await route.fulfill({ status: response.status, contentType: 'application/json', body: await response.text() });
      } finally { globalThis.fetch = original; }
    });
    // Hold success navigation: repeat submits must remain blocked until page departure. The Navigation API
    // cancels the departure inside the page; a route-fulfilled 204 raced Playwright's pending-navigation wait.
    if (mode === 'duplicate') await page.addInitScript(() => {
      (window as any).navigation.addEventListener('navigate', (e: any) => {
        if (e.destination.url.includes('sent=1')) { e.preventDefault(); (window as any).__sentNav = e.destination.url; }
      });
    });
    await page.goto('/contact/');
    await page.locator('[name="name"]').fill('CI');
    await page.locator('[name="company"]').fill('Example');
    await page.locator('[name="email"]').fill('ci@example.invalid');
    await page.locator('[name="message"]').fill('Synthetic RFQ');
    await page.locator('button[type="submit"]').click();
    if (mode === 'success') await expect(page).toHaveURL(/\/contact\/\?sent=1$/);
    else if (mode === 'duplicate') {
      await expect.poll(() => page.evaluate(() => (window as any).__sentNav)).toContain('sent=1');
      await expect(page.locator('.form-status')).toHaveText(/received/);
      expect(await page.evaluate(() => document.querySelector<HTMLButtonElement>('button[type="submit"]')!.disabled)).toBe(true);
      await page.evaluate(() => document.querySelector('form.rfq')!.dispatchEvent(new Event('submit', { cancelable: true })));
      expect(calls).toBe(1);
    } else {
      await expect(page.locator('.form-status')).toBeFocused();
      await expect(page.locator('button[type="submit"]')).toBeEnabled();
      await expect(page.locator('textarea')).toHaveValue('Synthetic RFQ');
      await expect(page.locator('.form-status')).not.toHaveText(/received/);
    }
    expect(writes).toBe(mode === 'validation' ? 0 : 1);
    expect(logs.join('\n')).not.toMatch(/ci@example.invalid|Synthetic RFQ|ci-private-placeholder/);
  });
}
