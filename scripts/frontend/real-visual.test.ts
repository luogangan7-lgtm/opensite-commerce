/** CI inventory gate: missing required real-backend evidence is always RED.
 * Depends on the explicit issue manifest and committed PNG/provenance files.
 */
import { expect, test } from 'bun:test';
import { existsSync, readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { BASELINES, sha256 } from './baseline-provenance';
import { REAL_VISUAL_STATES, realVisualPaths, realBackendEvidence } from './real-visual-manifest';
import { assertDistinctHistoryPngs, assertDistinctStaffPngs, SHARED_RENDER } from './real-visual-shared-render-manifest';

test('VISUAL-230 runtime masks cannot compare their geometry with itself', () => {
  const source = readFileSync('tests/frontend/real-backend.spec.ts', 'utf8');
  const calls = [...source.matchAll(/assertDateMaskBounds\(([^;]+)\);/g)];
  for (const call of calls) {
    const args = call[1].split(',').map(value => value.trim());
    expect(args[0], `Self-comparing mask bounds: ${call[0]}`).not.toBe(args[1]);
  }
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
  expect(source).toMatch(/const dateMasks\s*=\s*\[date\]/);
  expect(source).toMatch(/assertDateMaskBounds\(await maskBox\(dateMasks\[0\]\)/);
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
    'scripts/frontend/real-proxy.ts': '76076331f022df650544b3a465952ef9b19cd78101a6e3c861b073d338b67189',
    'scripts/frontend/start-real-backend.py': '3172947dbf65576660dba3031f56a9cc3de619de7ad84ac6a3ebe105a6f03831',
  };
  for (const [path, digest] of Object.entries(approved)) expect(sha256(path), path).toBe(digest);
});

test('VISUAL-REAL cleanup continues after a failed database drop', () => {
  const result = spawnSync('python3', ['scripts/frontend/test_real_launcher.py',
    'LauncherCleanup.test_drop_database_failure_still_attempts_each_role'], { encoding: 'utf8' });
  expect(result.status, result.stderr).toBe(0);
});
