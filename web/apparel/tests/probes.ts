/** CI fault probes use disposable copies; each mutation must turn the real output gate red. */
import { cpSync, mkdirSync, mkdtempSync, rmSync, appendFileSync, writeFileSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
for (const [name, mutation] of [['brand', '<p>mgoo</p>'], ['commercial', '<p>MOQ: 100</p>'], ['worker-format', 'Content-Disposition: form-data; name=metadata'], ['direction', ''], ['hreflang', '']] as const) {
  const dir = mkdtempSync(join(tmpdir(), 'apparel-probe-'));
  try {
    cpSync('web/apparel/dist', dir, { recursive: true });
    if (name === 'worker-format') writeFileSync(join(dir, '_worker.js/index.js'), mutation);
    else if (name === 'direction' || name === 'hreflang') {
      const file = join(dir, 'ar/index.html');
      const original = readFileSync(file, 'utf8');
      writeFileSync(file, name === 'direction' ? original.replace('dir="rtl"', 'dir="ltr"') : original.replace(/<link rel="alternate" hreflang="es"[^>]*>/, ''));
    }
    else appendFileSync(join(dir, 'index.html'), mutation);
    const red = Bun.spawnSync(['bun', 'web/apparel/tests/dist-gate.ts'], { env: { ...process.env, APPAREL_DIST: dir } });
    if (red.exitCode === 0) throw Error(`Gate missed ${name}`);
    console.log(`RED proven: ${name}`);
  } finally { rmSync(dir, { recursive: true, force: true }); }
}
const green = Bun.spawnSync(['bun', 'web/apparel/tests/dist-gate.ts'], { stdout: 'inherit', stderr: 'inherit' });
if (green.exitCode) throw Error('Unmodified dist failed after probes');

// Isolated round-4 probe: remove a real built slot; the slot contract must detect it.
{
  const dir = mkdtempSync(join(tmpdir(), 'apparel-slot-probe-'));
  try {
    cpSync('web/apparel/dist', dir, { recursive: true });
    const file = join(dir, 'index.html');
    writeFileSync(file, readFileSync(file, 'utf8').replace('data-slot="home/hero"', 'data-slot="home/missing"'));
    const red = Bun.spawnSync(['bun', 'web/apparel/tests/slots-gate.ts'], { env: { ...process.env, APPAREL_DIST: dir } });
    if (red.exitCode === 0 || !red.stderr.toString().includes('missing slot or localized alt home/hero')) throw Error('Slot probe did not cause the expected RED');
    console.log('ISOLATED round4 missing-slot RED proven');
  } finally { rmSync(dir, { recursive: true, force: true }); }
}
// Isolated round-4 probe: wrongly accept failed review rows in a disposable source copy.
{
  const dir = mkdtempSync(join(tmpdir(), 'apparel-manifest-probe-'));
  try {
    mkdirSync(join(dir, 'src/data'), { recursive: true }); mkdirSync(join(dir, 'tests'));
    cpSync('web/apparel/src/data/imagery.ts', join(dir, 'src/data/imagery.ts'));
    cpSync('web/apparel/tests/imagery.test.ts', join(dir, 'tests/imagery.test.ts'));
    writeFileSync(join(dir, 'image-plan.ts'), readFileSync('web/apparel/image-plan.ts', 'utf8').replace('row.pass === true', 'true'));
    const red = Bun.spawnSync(['bun', 'test', join(dir, 'tests/imagery.test.ts')]);
    if (red.exitCode === 0 || !red.stderr.toString().includes('(fail) manifest uses exactly approved packshots')) throw Error('Manifest probe did not cause the expected RED');
    console.log('ISOLATED round4 failed-review manifest RED proven');
  } finally { rmSync(dir, { recursive: true, force: true }); }
}
const restored = Bun.spawnSync(['bun', 'test', 'web/apparel/tests/imagery.test.ts'], { stdout: 'inherit', stderr: 'inherit' });
if (restored.exitCode) throw Error('Clean manifest contract failed after probes');
