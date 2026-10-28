/** Owns per-PNG golden provenance (producer run, source SHA, pins, PNG hash); CI writer/verifier call it.
 * Invariant (C-08d): provenance says who rendered a golden, never whether current code still matches it —
 * that is the candidate comparison's job. The input fingerprint is producer-time evidence, not an equality gate. */
import { createHash } from 'node:crypto';
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { scopeInputs } from './scoped-inputs';
import { realBackendEvidence } from './real-visual-manifest';

export const PLAYWRIGHT_IMAGE = 'mcr.microsoft.com/playwright:v1.64.0-noble@sha256:bc72a8df40831ded821e3cbc79c318c21b44d6e51dff9fd89ace7a8249c5b462';
export const BASELINES = 'tests/frontend/baselines';
export const REPOSITORY = 'luogangan7-lgtm/opensite-commerce';
export const SCOPES = ['gallery', 'admin', 'storefront-published', 'storefront-commerce', 'real-backend'] as const;
export type Scope = typeof SCOPES[number];
export const SPECS: Record<Scope, string> = {
  gallery: 'tests/frontend/gallery.spec.ts', admin: 'tests/frontend/admin.spec.ts',
  'storefront-published': 'tests/frontend/storefront.spec.ts',
  'storefront-commerce': 'tests/frontend/storefront-commerce.spec.ts',
  'real-backend': 'tests/frontend/real-backend.spec.ts',
};
// chromiumArgs (C-08e): one producer job rendered storefront-commerce-cart.png twice (compare, then update) on the
// same host and got two variants differing ±1 per channel at the anti-aliased corners of the just-blurred
// quantity input only. Pixels depended on repaint history: cc partial raster re-rasters only the invalidated
// rect of a reused tile. Full-tile raster, CPU raster and an sRGB output profile make pixels a function of the DOM.
export const RENDERING = {
  locale: 'en-US', timezone: 'UTC', deviceScaleFactor: 1, colorScheme: 'light', reducedMotion: 'reduce',
  desktop: [1280, 800], mobile: [390, 844],
  chromiumArgs: ['--disable-partial-raster', '--disable-gpu', '--force-color-profile=srgb'],
};
export type BaselineRecord = {
  schema: 2; snapshot: string; scope: Scope; sourceFingerprint: string; pngSha256: string;
  scopeSnapshots: string[];
  sourceSha: string; runUrl: string; image: string; playwright: string; font: string; rendering: typeof RENDERING;
  backend?: ReturnType<typeof realBackendEvidence>;
};
export function filesAt(directory: string): string[] {
  return readdirSync(directory).sort().flatMap(name => {
    const path = join(directory, name);
    if (['node_modules', 'dist', 'dist-gallery', '.astro', 'baselines'].includes(name)) return [];
    return statSync(path).isDirectory() ? filesAt(path) : [path];
  });
}
export function sha256(path: string) { return createHash('sha256').update(readFileSync(path)).digest('hex'); }
/** Stable identity of an input: migrations drop their (CI-renumbered, INF-MIG) numeric prefix and pending/ directory. */
export function inputKey(path: string): string {
  const pending = /^migrations\/pending\/([^/]+\.sql)$/.exec(path);
  if (pending) return `migrations/${pending[1]}`;
  const numbered = /^migrations\/\d+_([^/]+\.sql)$/.exec(path);
  return numbered ? `migrations/${numbered[1]}` : path;
}
export function fingerprintOf(entries: [path: string, sha256: string][]): string {
  const lines = entries.map(([path, hash]) => `${inputKey(path)}\0${hash}\n`).sort();
  return createHash('sha256').update(lines.join('')).digest('hex');
}
export function sourceFingerprint(scope: Scope, root = process.cwd()) {
  return fingerprintOf(scopeInputs(scope, root).map(path => [path, sha256(join(root, path))]));
}
export function fingerprints(root = process.cwd()): Record<Scope, string> {
  return Object.fromEntries(SCOPES.map(scope => [scope, sourceFingerprint(scope, root)])) as Record<Scope, string>;
}
export function scopeForSnapshot(snapshot: string): Scope {
  const match = /^(desktop|mobile)\/([a-z0-9-]+)\.png$/.exec(snapshot);
  if (!match) throw new Error(`Invalid PNG identity: ${snapshot}`);
  const name = match[2];
  if (name.startsWith('real-')) return 'real-backend';
  if (name.startsWith('admin-')) return 'admin';
  if (name.startsWith('storefront-commerce-')) return 'storefront-commerce';
  if (name.startsWith('storefront-')) return 'storefront-published';
  if (/^(gallery|(?:button|input|select|checkbox|field|badge|status|card|table|empty|dialog)-[a-z0-9-]+)$/.test(name)) return 'gallery';
  throw new Error(`Unknown PNG input scope: ${snapshot}`);
}
export function recordPath(snapshot: string) { scopeForSnapshot(snapshot); return `${snapshot}.provenance.json`; }
export function runId(runUrl: string): string {
  const match = /^https:\/\/github\.com\/luogangan7-lgtm\/opensite-commerce\/actions\/runs\/([1-9][0-9]*)$/.exec(runUrl);
  if (!match) throw new Error('Invalid baseline producer run URL');
  return match[1];
}
export function makeRecord(snapshot: string, fingerprint: string, png: string, sourceSha: string, runUrl: string, scopeSnapshots = [snapshot]): BaselineRecord {
  return { schema: 2, snapshot, scope: scopeForSnapshot(snapshot), sourceFingerprint: fingerprint, pngSha256: png,
    scopeSnapshots, sourceSha, runUrl, image: PLAYWRIGHT_IMAGE, playwright: '1.64.0', font: '@fontsource/inter@5.3.0', rendering: RENDERING,
    ...(scopeForSnapshot(snapshot) === 'real-backend' ? { backend: realBackendEvidence(snapshot) } : {}) };
}
export function assertBaselineSet(pngs: Record<string, string>, records: Record<string, unknown>): BaselineRecord[] {
  const pngPaths = Object.keys(pngs).sort();
  const recordPaths = Object.keys(records).sort();
  if (JSON.stringify(pngPaths) !== JSON.stringify(recordPaths)) throw new Error('Missing or orphan PNG provenance record');
  return pngPaths.map(snapshot => {
    const value = records[snapshot];
    if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid PNG provenance record');
    const record = value as BaselineRecord;
    if (record.schema !== 2 || record.snapshot !== snapshot || record.scope !== scopeForSnapshot(snapshot)) throw new Error(`Invalid PNG scope or schema: ${snapshot}`);
    if (record.scope === 'real-backend' && JSON.stringify(record.backend) !== JSON.stringify(realBackendEvidence(snapshot))) throw new Error(`Missing or altered real backend/fault provenance: ${snapshot}`);
    const inventory = pngPaths.filter(path => scopeForSnapshot(path) === record.scope);
    if (!Array.isArray(record.scopeSnapshots) || JSON.stringify(record.scopeSnapshots) !== JSON.stringify(inventory)) throw new Error(`Producer-observed scope inventory differs: ${snapshot}`);
    if (typeof record.sourceSha !== 'string' || !/^[0-9a-f]{40}$/.test(record.sourceSha)) throw new Error('Invalid producer SHA');
    runId(record.runUrl);
    if (record.image !== PLAYWRIGHT_IMAGE || record.playwright !== '1.64.0' || record.font !== '@fontsource/inter@5.3.0' ||
        JSON.stringify(record.rendering) !== JSON.stringify(RENDERING)) throw new Error(`Unpinned PNG rendering: ${snapshot}`);
    if (typeof record.sourceFingerprint !== 'string' || !/^[0-9a-f]{64}$/.test(record.sourceFingerprint)) throw new Error(`Invalid producer input fingerprint: ${snapshot}`);
    if (!/^[0-9a-f]{64}$/.test(record.pngSha256) || record.pngSha256 !== pngs[snapshot]) throw new Error(`PNG hash differs: ${snapshot}`);
    return record;
  });
}
export function assertRunAuthority(record: Pick<BaselineRecord, 'runUrl' | 'sourceSha'>, value: unknown) {
  const run = value as { id?: number; head_sha?: string; event?: string; path?: string; repository?: { full_name?: string } };
  if (!run || String(run.id) !== runId(record.runUrl)) throw new Error('Baseline producer run differs');
  if (run.head_sha !== record.sourceSha) throw new Error('Golden source SHA does not match CI run');
  if (run.event !== 'push' || run.path !== '.github/workflows/checks.yml' || run.repository?.full_name !== REPOSITORY) throw new Error('Unexpected baseline producer run authority');
}
export function pngHashes(root = process.cwd()): Record<string, string> {
  const directory = join(root, BASELINES);
  return Object.fromEntries(['desktop', 'mobile'].flatMap(project => readdirSync(join(directory, project)).filter(name => name.endsWith('.png')).sort()
    .map(name => [`${project}/${name}`, sha256(join(directory, project, name))])));
}
export function readRecords(root = process.cwd()): Record<string, unknown> {
  const directory = join(root, BASELINES);
  if (existsSync(join(directory, 'provenance.json'))) throw new Error('Legacy global provenance requires a CI baseline migration');
  return Object.fromEntries(filesAt(directory).filter(path => path.endsWith('.png.provenance.json')).map(path => {
    const snapshot = relative(directory, path).replaceAll('\\', '/').replace(/\.provenance\.json$/, '');
    scopeForSnapshot(snapshot);
    return [snapshot, JSON.parse(readFileSync(path, 'utf8'))];
  }));
}
export function checkedRecords(root = process.cwd()): BaselineRecord[] {
  const records = assertBaselineSet(pngHashes(root), readRecords(root));
  for (const scope of SCOPES) {
    for (const viewport of ['desktop', 'mobile']) if (!records.some(record => record.scope === scope && record.snapshot.startsWith(`${viewport}/`))) throw new Error(`Missing ${viewport} scope inventory: ${scope}`);
  }
  return records;
}
/** Maps `git diff --name-only` paths to the golden snapshots whose PNG or record bytes changed. */
export function changedSnapshots(paths: string[]): Set<string> {
  return new Set(paths.filter(path => path.startsWith(`${BASELINES}/`))
    .map(path => path.slice(BASELINES.length + 1).replace(/\.provenance\.json$/, ''))
    .filter(snapshot => /^(desktop|mobile)\/[a-z0-9-]+\.png$/.test(snapshot)));
}
/**
 * Goldens to verify against their producer artifact. `HEAD^1` is the pull_request merge-ref base; on push the
 * base is `github.event.before`, which must be an ancestor of HEAD so every pushed commit is covered. A zero,
 * empty, unknown or non-ancestor (force-push) base fails closed to 'all'.
 */
export function goldenChangeSet(base: string, cwd = process.cwd()): Set<string> | 'all' {
  const git = (...args: string[]) => Bun.spawnSync(['git', ...args], { cwd, stdout: 'pipe', stderr: 'pipe' });
  if (base !== 'HEAD^1') {
    if (!/^[0-9a-f]{40}$/.test(base) || /^0+$/.test(base)) return 'all';
    if (git('cat-file', '-e', `${base}^{commit}`).exitCode !== 0) {
      git('fetch', '--no-tags', '--quiet', 'origin', base);
      if (git('cat-file', '-e', `${base}^{commit}`).exitCode !== 0) return 'all';
    }
    if (git('merge-base', '--is-ancestor', base, 'HEAD').exitCode !== 0) return 'all';
  }
  const diff = git('diff', '--name-only', '--no-renames', base, 'HEAD', '--', BASELINES);
  if (diff.exitCode !== 0) throw new Error(`Cannot diff goldens against ${base}`);
  return changedSnapshots(new TextDecoder().decode(diff.stdout).split('\n').filter(Boolean));
}
