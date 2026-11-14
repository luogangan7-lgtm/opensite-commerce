/** Mutate disposable real dist copies; wrong locale and invented fact must fail the output gate. */
import { cpSync, mkdtempSync, readFileSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { publishedPaths } from '../seo/published';
for (const name of ['locale', 'fact', 'placeholder']) {
  const dir = mkdtempSync(join(tmpdir(), 'apparel-seo-'));
  try {
    cpSync('web/apparel/dist', dir, { recursive: true });
    const file = join(dir, name === 'locale' ? 'index.html' : publishedPaths.find(p => p.startsWith('/products/'))!.slice(1) + 'index.html');
    const html = readFileSync(file, 'utf8');
    const changed = name === 'locale' ? html.replace('hreflang="zh-Hans"', 'hreflang="xx"') : name === 'placeholder' ? html.replace('</body>', '<p>TBD</p></body>') : html.replace('"@type":"Product"', '"@type":"Product","material":"invented cotton"');
    if (html === changed) throw Error('Mutation did not apply');
    writeFileSync(file, changed);
    const result = Bun.spawnSync(['bun', 'web/apparel/tests/seo-output.ts'], { env: { ...process.env, APPAREL_DIST: dir } });
    if (result.exitCode === 0) throw Error(`Missed ${name}`);
    const error = result.stderr.toString();
    const expected = name === 'locale' ? 'Wrong reciprocal alternates' : name === 'placeholder' ? 'Product placeholder' : 'Unsupported product fact';
    if (!error.includes(expected)) throw Error(error);
    console.log(`RED proven: ${name}; ${expected}`);
  } finally { rmSync(dir, { recursive: true, force: true }); }
  const green = Bun.spawnSync(['bun', 'web/apparel/tests/seo-output.ts'], { stdout: 'inherit', stderr: 'inherit' });
  if (green.exitCode) throw Error('Restored output failed');
}

// Post-build red/green evidence against disposable copies of the real output.
const testOrigin = 'https://seo-gate.example.org'; // synthetic fixture, never an owner domain
function gate(dir: string, mode: string, origin: string | undefined, expected: string | null) {
  const env = { ...process.env, APPAREL_DIST: dir, SEO_BUILD_MODE: mode, PRODUCTION_ORIGIN: origin };
  const result = Bun.spawnSync(['bun', 'web/apparel/tests/seo-output.ts'], { env });
  if (expected ? result.exitCode === 0 || !result.stderr.toString().includes(expected) : result.exitCode !== 0) throw Error(`Probe mismatch ${mode}/${expected}: ${result.stderr}`);
  console.log(`${expected ? 'RED' : 'GREEN'} proven: ${mode} ${expected || 'valid output'}`);
}
for (const [name, expected] of [['html', 'HTML robots policy'], ['header', 'X-Robots-Tag policy'], ['robots', 'Crawler policy'], ['origin', 'OG URL']] as const) {
  const dir = mkdtempSync(join(tmpdir(), 'apparel-seo-policy-'));
  try {
    cpSync('web/apparel/dist', dir, { recursive: true });
    const file = join(dir, name === 'header' ? '_headers' : name === 'robots' ? 'robots.txt' : 'index.html');
    const original = readFileSync(file, 'utf8');
    const changed = name === 'html' ? original.replace(/<meta name="robots" content="noindex"\s*\/?>/, '') : name === 'header' ? original.replace(/.*X-Robots-Tag:.*\n?/, '') : name === 'robots' ? original.replace('Allow: /', 'Disallow: /') : original.replaceAll('https://sourcyowear.example.invalid', testOrigin);
    if (changed === original) throw Error(`Mutation did not apply: ${name}`);
    writeFileSync(file, changed);
    gate(dir, 'preview', undefined, expected);
    writeFileSync(file, original);
    gate(dir, 'preview', undefined, null);
  } finally { rmSync(dir, { recursive: true, force: true }); }
}
gate('web/apparel/dist', 'production', 'https://sourcyowear.example.invalid', 'Unsafe PRODUCTION_ORIGIN');
for (const origin of [undefined, 'http://seo-gate.example.org', 'https://localhost', 'https://x.pages.dev']) gate('web/apparel/dist', 'production', origin, origin ? 'Unsafe PRODUCTION_ORIGIN' : 'PRODUCTION_ORIGIN required');
const dir = mkdtempSync(join(tmpdir(), 'apparel-seo-production-'));
try {
  cpSync('web/apparel/dist', dir, { recursive: true });
  for await (const path of new Bun.Glob('**/*.{html,xml,txt}').scan(dir)) {
    const file = join(dir, path);
    writeFileSync(file, readFileSync(file, 'utf8').replaceAll('https://sourcyowear.example.invalid', testOrigin).replace(/<meta name="robots" content="noindex"\s*\/?>/g, '').replace('Disallow: /', 'Allow: /'));
  }
  const headers = join(dir, '_headers');
  writeFileSync(headers, readFileSync(headers, 'utf8').replace(/.*X-Robots-Tag:.*\n?/, ''));
  gate(dir, 'production', testOrigin, null);
  for (const [name, expected] of [['index.html', 'HTML robots policy'], ['_headers', 'X-Robots-Tag policy'], ['robots.txt', 'Crawler policy']] as const) {
    const file = join(dir, name), original = readFileSync(file, 'utf8');
    writeFileSync(file, name === 'index.html' ? original.replace('</head>', '<meta name="robots" content="noindex"></head>') : name === '_headers' ? original + '  X-Robots-Tag: noindex\n' : original.replace('Allow: /', 'Disallow: /'));
    gate(dir, 'production', testOrigin, expected);
    writeFileSync(file, original);
    gate(dir, 'production', testOrigin, null);
  }
} finally { rmSync(dir, { recursive: true, force: true }); }
