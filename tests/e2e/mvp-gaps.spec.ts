/** Responsibility: expose whole MVP browser journeys that current main cannot execute.
 * Dependencies: real pending feature PRs; caller: CI E2E and acceptance matrix.
 * Invariants: FIXME is never PASS; removing it requires a real scenario, not an empty body.
 */
import { test } from '@playwright/test';

test('FIXME staff login → create SKU → media upload → publish', async () => {
  test.fixme(true, 'PR #43 staff login and PR #45 publishing are OPEN; main has no working staff-login/publish UI chain');
  throw new Error('Replace this gap with the complete real browser journey and persisted API assertions before removing FIXME');
});

test('FIXME guest storefront buy → cart → checkout → Fakepay', async () => {
  test.fixme(true, 'PR #55 public SKU wiring, PR #38 checkout and PR #53 storefront purchasing are OPEN');
  throw new Error('Replace this gap with the complete real browser journey and captured payment/order assertions before removing FIXME');
});
