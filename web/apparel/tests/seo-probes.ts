/** Mutate disposable real dist copies; wrong locale and invented fact must fail the output gate. */
import { cpSync, mkdtempSync, readFileSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { publishedPaths } from '../seo/published';
for (const name of ['locale', 'fact']) {
  const dir = mkdtempSync(join(tmpdir(), 'apparel-seo-'));
  try {
    cpSync('web/apparel/dist', dir, { recursive: true });
    const file = join(dir, name === 'locale' ? 'index.html' : publishedPaths.find(p => p.startsWith('/products/'))!.slice(1) + 'index.html');
    const html = readFileSync(file, 'utf8');
    const changed = name === 'locale' ? html.replace('hreflang="zh-Hans"', 'hreflang="xx"') : html.replace('"@type":"Product"', '"@type":"Product","material":"invented cotton"');
    if (html === changed) throw Error('Mutation did not apply');
    writeFileSync(file, changed);
    const result = Bun.spawnSync(['bun', 'web/apparel/tests/seo-output.ts'], { env: { ...process.env, APPAREL_DIST: dir } });
    if (result.exitCode === 0) throw Error(`Missed ${name}`);
    const error = result.stderr.toString();
    if (!error.includes(name === 'locale' ? 'Wrong reciprocal alternates' : 'Unsupported product fact')) throw Error(error);
    console.log(`RED proven: ${name}; ${name === 'locale' ? 'Wrong reciprocal alternates' : 'Unsupported product fact'}`);
  } finally { rmSync(dir, { recursive: true, force: true }); }
  const green = Bun.spawnSync(['bun', 'web/apparel/tests/seo-output.ts'], { stdout: 'inherit', stderr: 'inherit' });
  if (green.exitCode) throw Error('Restored output failed');
}
