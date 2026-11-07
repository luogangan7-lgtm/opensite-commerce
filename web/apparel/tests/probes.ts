/** CI fault probes use disposable copies; each mutation must turn the real output gate red. */
import { cpSync, mkdtempSync, rmSync, appendFileSync, writeFileSync, readFileSync } from 'node:fs';
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
