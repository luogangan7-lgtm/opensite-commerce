/** CI fault probes use disposable copies; each mutation must turn the real output gate red. */
import { cpSync, mkdtempSync, rmSync, appendFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
for (const [name, mutation] of [['brand', '<p>mgoo</p>'], ['commercial', '<p>MOQ: 100</p>']] as const) {
  const dir = mkdtempSync(join(tmpdir(), 'apparel-probe-'));
  try {
    cpSync('web/apparel/dist', dir, { recursive: true }); appendFileSync(join(dir, 'index.html'), mutation);
    const red = Bun.spawnSync(['bun', 'web/apparel/tests/dist-gate.ts'], { env: { ...process.env, APPAREL_DIST: dir } });
    if (red.exitCode === 0) throw Error(`Gate missed ${name}`);
    console.log(`RED proven: ${name}`);
  } finally { rmSync(dir, { recursive: true, force: true }); }
}
const green = Bun.spawnSync(['bun', 'web/apparel/tests/dist-gate.ts'], { stdout: 'inherit', stderr: 'inherit' });
if (green.exitCode) throw Error('Unmodified dist failed after probes');
