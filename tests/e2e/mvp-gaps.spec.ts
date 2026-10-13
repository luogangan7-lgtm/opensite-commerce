/** Responsibility: expose whole MVP browser journeys that current main cannot execute.
 * Dependencies: real pending feature PRs; caller: CI E2E and acceptance matrix.
 * Invariants: FIXME is never PASS; removing it requires a real scenario, not an empty body.
 */
import { test } from '@playwright/test';

test('FIXME staff create SKU → media upload → publish', async () => {
  test.fixme(true, 'Staff login is covered in admin.spec.ts; PR #45 CMS publish and PR #55 storefront SKUs are OPEN');
  throw new Error('Replace this gap with the complete real browser journey and persisted API assertions before removing FIXME');
});

test('FIXME authenticated admin catalog list and create', async () => {
  test.fixme(true, 'Login works, but main wires the authenticator only for /api/v1/auth/*; private tenant routes still return 401 for a valid staff session (follow-up: authorize private routes with the staff session)');
  throw new Error('Replace this gap with real catalog list/create assertions as the logged-in owner before removing FIXME');
});

test('FIXME admin orders list and detail on the real orders read API', async () => {
  test.fixme(true, 'Orders read API (#47) is merged but the admin UI still serves dev-only fixtures (ordersReadAvailable in web/admin/src/app.tsx); follow-up must switch it to the real API');
  throw new Error('Replace this gap with real orders list/detail assertions against seeded orders before removing FIXME');
});

test('FIXME guest storefront buy → cart → checkout → Fakepay', async () => {
  test.fixme(true, 'PR #55 public SKU wiring and PR #38 checkout are OPEN (storefront purchase pages #53 are merged)');
  throw new Error('Replace this gap with the complete real browser journey and captured payment/order assertions before removing FIXME');
});
