/** CI inventory gate: missing required real-backend evidence is always RED.
 * Depends on the explicit issue manifest and committed PNG/provenance files.
 */
import { expect, test } from 'bun:test';
import { existsSync, readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { BASELINES, sha256 } from './baseline-provenance';
import { REAL_VISUAL_STATES, realVisualPaths, realBackendEvidence, assertDistinctCatalogPngs } from './real-visual-manifest';
import { assertDistinctHistoryPngs, assertDistinctStaffPngs, SHARED_RENDER } from './real-visual-shared-render-manifest';

test('VISUAL-230 runtime masks resolve independent locators and measure their own geometry', () => {
  const source = readFileSync('tests/frontend/real-backend.spec.ts', 'utf8');
  expect(source).toContain('async function assertDateMaskBounds(mask: Locator, bounds: Locator, protectedBoxes: Box[])');
  const checker = source.slice(source.indexOf('async function assertDateMaskBounds'), source.indexOf('// Keep the exact existing mask rectangle'));
  expect(checker).toContain('await bounds.elementHandle()');
  expect(checker).toContain('element === other');
  expect(checker).toContain('Mask and bounds resolve to the same element');
  expect(checker).toContain('mask.boundingBox()');
  expect(checker).toContain('bounds.boundingBox()');
  // Deliberate alias negatives belong only to the structural regression below.
  const callers = source.replace(/test\('VISUAL-274 rejects oversized masks and aliases using independent DOM bounds'[\s\S]*?\n\}\);/, '');
  const calls = [...callers.matchAll(/await (?:expect\()?assertDateMaskBounds\(([^,\n]+), ([^,\n]+), /g)];
  expect(calls.length).toBeGreaterThan(0);
  for (const call of calls) {
    expect(call[1], `Self-comparing mask bounds: ${call[0]}`).not.toBe(call[2]);
    expect(`${call[1]} ${call[2]}`, `Precomputed mask geometry: ${call[0]}`).not.toMatch(/\b(?:box|\w+Box|\w+Bounds)\b|maskBox\(/);
  }
  const runtime = source.slice(source.indexOf('async function runtimeMask('), source.indexOf('// This candidate extends only one pixel'));
  expect(runtime).toContain('await assertDateMaskBounds(target, field, protectedBoxes)');
  expect(runtime).not.toMatch(/target\.boundingBox\(\)|field\.boundingBox\(\)/);
  expect(source).not.toMatch(/runtimeMask\(page, (metadata|completionID), \1\)/);
});

test('VISUAL-230 multiline fixture snapshots the actual catalog variant through Orders', () => {
  const source = readFileSync('tests/frontend/real-backend.spec.ts', 'utf8');
  const multiline = source.slice(source.indexOf("test('real multiline order text"), source.indexOf('// Masks own only'));
  expect(multiline).toContain('domainMultilineOrder');
  expect(multiline).toContain('snapshot.sku_code');
  const seed = source.slice(source.indexOf('function domainMultilineOrder'), source.indexOf('async function staffLogin'));
  expect(seed).toContain('staffOrders(0)');
  expect(seed).toContain('app.orders_create(');
  expect(seed).toContain("'sku',v.sku");
  expect(seed).not.toContain('INSERT INTO app.order_lines');
});

test('VISUAL-REAL requires every issue state in desktop and mobile', () => {
  for (const snapshot of realVisualPaths()) {
    const path = `${BASELINES}/${snapshot}`;
    expect(existsSync(path), `Missing required real-backend baseline: ${snapshot}`).toBe(true);
    const record = JSON.parse(readFileSync(`${path}.provenance.json`, 'utf8'));
    expect(record.pngSha256).toBe(sha256(path));
    expect(record.backend).toMatchObject({ application: 'Go', database: 'PostgreSQL 18.6', mode: 'test' });
    expect(record.backend).toEqual(realBackendEvidence(snapshot));
  }
});

test('VISUAL-250 required states have distinct PNG bytes outside shared render groups', () => {
  const pngs = Object.fromEntries(['desktop', 'mobile'].flatMap(viewport =>
    REAL_VISUAL_STATES[250].map(state => {
      const snapshot = `${viewport}/real-250-${state}.png`;
      return [snapshot, sha256(`${BASELINES}/${snapshot}`)];
    })));
  assertDistinctHistoryPngs(pngs);
});

test('VISUAL-250 allows only declared shared renders and rejects undeclared duplicate or missing images', () => {
  const pngs = Object.fromEntries(['desktop', 'mobile'].flatMap(viewport =>
    REAL_VISUAL_STATES[250].map(state => [`${viewport}/real-250-${state}.png`, `${viewport}-${state}`])));
  for (const viewport of ['desktop', 'mobile']) {
    for (const state of ['history-detail-loading', 'history-pagination-loading'])
      pngs[`${viewport}/real-250-${state}.png`] = pngs[`${viewport}/real-250-history-loading.png`];
  }
  expect(() => assertDistinctHistoryPngs(pngs)).not.toThrow();
  const duplicate = { ...pngs, 'desktop/real-250-history-empty.png': pngs['desktop/real-250-history-list.png'] };
  expect(() => assertDistinctHistoryPngs(duplicate)).toThrow();
  const missing = { ...pngs, 'mobile/real-250-history-empty.png': '' };
  expect(() => assertDistinctHistoryPngs(missing)).toThrow();
});

test('VISUAL-250 rejects every data state in a shared render group', () => {
  const pngs = Object.fromEntries(['desktop', 'mobile'].flatMap(viewport =>
    REAL_VISUAL_STATES[250].map(state => [`${viewport}/real-250-${state}.png`, `${viewport}-${state}`])));
  const groups = SHARED_RENDER as Record<string, string>;
  for (const state of REAL_VISUAL_STATES[250].filter(state => !state.endsWith('-loading'))) {
    try {
      groups[state] = 'history-loading-skeleton';
      expect(() => assertDistinctHistoryPngs(pngs), state).toThrow();
    } finally { delete groups[state]; }
  }
});

test('VISUAL-250 validates the exact locator used for date screenshot masks', () => {
  const source = readFileSync('tests/frontend/real-backend.spec.ts', 'utf8');
  expect(source).toMatch(/const dateMasks\s*=\s*\[await maskFieldContents\(date\)\]/);
  expect(source).toMatch(/await assertDateMaskBounds\(dateMasks\[0\], date, \[statusBox, amountBox\]\)/);
  expect(source).toMatch(/historySnapshot\(page, 'b2b-paid-list', false, true, dateMasks, maskColor\)/);
});

test('VISUAL-REAL browser lane cannot intercept business routes', () => {
  const source = readFileSync('tests/frontend/real-backend.spec.ts', 'utf8');
  expect(source).not.toMatch(/\.route\s*\(|\.routeFromHAR\s*\(|admin-fixture|commerce-fixture/);
});

test('VISUAL-REAL provenance identifies injected faults and unreachable boundary states', () => {
  expect(realBackendEvidence('desktop/real-213-checkout-in-flight.png').fault).toContain('real Go POST');
  expect(realBackendEvidence('mobile/real-213-completion-error.png').fault).toContain('503');
  expect(realBackendEvidence('mobile/real-213-completion-long-order.png').boundary).toContain('maximum legal UUID');
});

// #230 inventory is independent of implementation so an omitted state fails CI.
test('VISUAL-230 requires real staff order evidence for both viewports', () => {
  const states = ['list', 'detail', 'long-list', 'long-detail', 'empty', 'pagination',
    'list-loading', 'detail-loading', 'list-503', 'order-detail-unavailable-503',
    'unauthenticated', 'cross-tenant', 'expired-session', 'permission-denied',
    'multiline', 'paid', 'partially-fulfilled', 'fulfilled', 'partially-refunded', 'refunded',
    'rma-cart', 'rma-checkout', 'rma-completion'];
  const inventory = (REAL_VISUAL_STATES as unknown as Record<number, readonly string[]>)[230] ?? [];
  expect(inventory).toEqual(states);
  for (const viewport of ['desktop', 'mobile']) for (const state of states) {
    const snapshot = `${viewport}/real-230-${state}.png`;
    expect(existsSync(`${BASELINES}/${snapshot}`), `Missing ${snapshot}`).toBe(true);
    expect(existsSync(`${BASELINES}/${snapshot}.provenance.json`), `Missing provenance for ${snapshot}`).toBe(true);
  }
});


test('VISUAL-230 distinct state gate rejects a missing PNG and a copied data state', () => {
  const hashes = Object.fromEntries(['desktop', 'mobile'].flatMap(viewport =>
    REAL_VISUAL_STATES[230].map(state => [`${viewport}/real-230-${state}.png`, `${viewport}-${state}`])));
  expect(() => assertDistinctStaffPngs(hashes)).not.toThrow();
  const missing = { ...hashes, 'mobile/real-230-cross-tenant.png': '' };
  expect(() => assertDistinctStaffPngs(missing)).toThrow('staff state images must exist and be distinct');
  const copied = { ...hashes, 'desktop/real-230-long-detail.png': hashes['desktop/real-230-detail.png'] };
  expect(() => assertDistinctStaffPngs(copied)).toThrow('staff state images must exist and be distinct');
});

test('VISUAL-230 committed PNGs cannot alias a different staff state', () => {
  const hashes = Object.fromEntries(['desktop', 'mobile'].flatMap(viewport =>
    REAL_VISUAL_STATES[230].map(state => {
      const path = `${viewport}/real-230-${state}.png`;
      return [path, sha256(`${BASELINES}/${path}`)];
    })));
  assertDistinctStaffPngs(hashes);
});

// These reviewed sources are part of the real-data boundary. Any change requires
// re-review before updating the digest, including alternate response constructors.
test('VISUAL-REAL proxy and launcher match the reviewed no-DTO sources', () => {
  const approved: Record<string, string> = {
    'scripts/frontend/real-proxy.ts': '62e5f5f1c8d89a6b078a028276358d90cf6a56c7da60528c0c0e61ac0a976506',
    'scripts/frontend/start-real-backend.py': '78ab157106ac920330a26effa6ae94b2072de9eeb05c88383a1ef9e84e5895c0',
  };
  for (const [path, digest] of Object.entries(approved)) expect(sha256(path), path).toBe(digest);
});

test('VISUAL-REAL cleanup continues after a failed database drop', () => {
  const result = spawnSync('python3', ['scripts/frontend/test_real_launcher.py',
    'LauncherCleanup.test_drop_database_failure_still_attempts_each_role'], { encoding: 'utf8' });
  expect(result.status, result.stderr).toBe(0);
});

// Independent owner inventory: fixtures and old routed PNGs cannot satisfy #274.
test('VISUAL-274 requires each real staff catalog state in both viewports', () => {
  const states = ['edit', 'access-loading', 'empty', 'invalid-amount', 'invalid-json', 'upload-failure',
    'price-conflict', 'content-conflict', 'listing-conflict', 'content-saving', 'long-title', 'long-slug',
    'long-json', 'network-error', 'permission-denied', 'draft-retained', 'reload-saved', 'price-editor',
    'published', 'attached-published', 'create', 'variant-saved', 'create-busy',
    'product-busy', 'variant-busy', 'invalid-uuid', 'uuid-recovered', 'invalid-key', 'key-recovered', 'reserve-failure', 'reserve-retry',
    'content-failure', 'content-retry', 'complete-failure', 'complete-retry', 'media-ready', 'empty-media', 'long-filename'];
  expect((REAL_VISUAL_STATES as unknown as Record<number, readonly string[]>)[274] ?? []).toEqual(states);
  for (const viewport of ['desktop', 'mobile']) for (const state of states) {
    const path = `${BASELINES}/${viewport}/real-274-${state}.png`;
    expect(existsSync(path), `Missing real #274 state ${path}`).toBe(true);
    expect(existsSync(`${path}.provenance.json`)).toBe(true);
  }
});

// The admin has one bundled copy dictionary and no locale selector/router.
test('VISUAL-274 supported admin locale list is English only (non-en row N/A)', () => {
  const html = readFileSync('web/admin/index.html', 'utf8');
  const supported = [...html.matchAll(/<html lang="([^"]+)"/g)].map(match => match[1]);
  expect(supported).toEqual(['en']);
  const copy = readFileSync('packages/component-contracts/src/index.ts', 'utf8');
  expect(copy).toContain('export function t(key: MessageKey): string { return messages[key]; }');
  expect(copy).not.toMatch(/export const (?:locales|translations|dictionaries)/);
});

test('VISUAL-274 rejects copied data-state and omitted viewport baselines', () => {
  const hashes = Object.fromEntries(['desktop', 'mobile'].flatMap(viewport =>
    REAL_VISUAL_STATES[274].map(state => [`${viewport}/real-274-${state}.png`, `${viewport}-${state}`])));
  expect(() => assertDistinctCatalogPngs(hashes)).not.toThrow();
  expect(() => assertDistinctCatalogPngs({ ...hashes, 'desktop/real-274-long-title.png': hashes['desktop/real-274-long-slug.png'] })).toThrow();
  expect(() => assertDistinctCatalogPngs({ ...hashes, 'mobile/real-274-invalid-uuid.png': '' })).toThrow();
});
test('VISUAL-274 committed data states have distinct pixels', () => {
  const hashes = Object.fromEntries(['desktop', 'mobile'].flatMap(viewport =>
    REAL_VISUAL_STATES[274].map(state => [`${viewport}/real-274-${state}.png`, sha256(`${BASELINES}/${viewport}/real-274-${state}.png`)])));
  assertDistinctCatalogPngs(hashes);
});
