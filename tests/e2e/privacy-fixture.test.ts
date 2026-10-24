/** CI browser regression for the actual automatic fixture and default page teardown. */
import { expect } from '@playwright/test';
import { fixture, test } from './fixture';

test('shared privacy fixture drains before its default page closes', async ({ page }) => {
  const response = await page.goto(fixture.storefront);
  expect(response?.status()).toBe(200);
});
