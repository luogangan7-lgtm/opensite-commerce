/** CI inventory gate: missing required real-backend evidence is always RED.
 * Depends on the explicit issue manifest and committed PNG/provenance files.
 */
import { expect, test } from 'bun:test';
import { existsSync, readFileSync } from 'node:fs';
import { BASELINES, sha256 } from './baseline-provenance';
import { realVisualPaths, realBackendEvidence } from './real-visual-manifest';

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

test('VISUAL-250 distinct history states have distinct PNG bytes', () => {
  for (const viewport of ['desktop', 'mobile']) {
    const states = ['history-list', 'history-long-order', 'history-detail', 'history-long-sku', 'history-multiline'];
    const hashes = states.map(state => sha256(`${BASELINES}/${viewport}/real-250-${state}.png`));
    expect(new Set(hashes).size, `${viewport} history state images must be distinct`).toBe(states.length);
  }
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
