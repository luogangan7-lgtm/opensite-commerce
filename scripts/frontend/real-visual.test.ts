/** CI inventory gate: missing required real-backend evidence is always RED.
 * Depends on the explicit issue manifest and committed PNG/provenance files.
 */
import { expect, test } from 'bun:test';
import { existsSync, readFileSync } from 'node:fs';
import { BASELINES, sha256 } from './baseline-provenance';
import { REAL_VISUAL_STATES, realVisualPaths, realBackendEvidence } from './real-visual-manifest';
import { assertDistinctHistoryPngs, SHARED_RENDER } from './real-visual-shared-render-manifest';

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
