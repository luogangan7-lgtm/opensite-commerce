/** CI inventory gate: missing required real-backend evidence is always RED.
 * Depends on the explicit issue manifest and committed PNG/provenance files.
 */
import { expect, test } from 'bun:test';
import { existsSync, readFileSync } from 'node:fs';
import { BASELINES, sha256 } from './baseline-provenance';
import { realVisualPaths } from './real-visual-manifest';

test('VISUAL-REAL requires every issue state in desktop and mobile', () => {
  for (const snapshot of realVisualPaths()) {
    const path = `${BASELINES}/${snapshot}`;
    expect(existsSync(path), `Missing required real-backend baseline: ${snapshot}`).toBe(true);
    const record = JSON.parse(readFileSync(`${path}.provenance.json`, 'utf8'));
    expect(record.pngSha256).toBe(sha256(path));
    expect(record.backend).toMatchObject({ application: 'Go', database: 'PostgreSQL 18.6', mode: 'test' });
  }
});
