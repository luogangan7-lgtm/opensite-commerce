/** Owns per-PNG integrity and renderer fingerprints; CI writer/verifier call it; artifacts never enter source hashes. */
import { createHash } from 'node:crypto';
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { scopeInputs } from './scoped-inputs';

export const PLAYWRIGHT_IMAGE = 'mcr.microsoft.com/playwright:v1.64.0-noble@sha256:bc72a8df40831ded821e3cbc79c318c21b44d6e51dff9fd89ace7a8249c5b462';
export const BASELINES = 'tests/frontend/baselines';
export const REPOSITORY = 'luogangan7-lgtm/opensite-commerce';
export const SCOPES = ['gallery', 'admin', 'storefront-published', 'storefront-commerce'] as const;
export type Scope = typeof SCOPES[number];
export const SPECS: Record<Scope, string> = {
  gallery: 'tests/frontend/gallery.spec.ts', admin: 'tests/frontend/admin.spec.ts',
  'storefront-published': 'tests/frontend/storefront.spec.ts',
  'storefront-commerce': 'tests/frontend/storefront-commerce.spec.ts',
};
export const RENDERING = {
  locale: 'en-US', timezone: 'UTC', deviceScaleFactor: 1, colorScheme: 'light', reducedMotion: 'reduce',
  desktop: [1280, 800], mobile: [390, 844],
};
export type BaselineRecord = {
  schema: 2; snapshot: string; scope: Scope; sourceFingerprint: string; pngSha256: string;
  sourceSha: string; runUrl: string; image: string; playwright: string; font: string; rendering: typeof RENDERING;
};
export function filesAt(directory: string): string[] {
  return readdirSync(directory).sort().flatMap(name => {
    const path = join(directory, name);
    if (['node_modules', 'dist', 'dist-gallery', '.astro', 'baselines'].includes(name)) return [];
    return statSync(path).isDirectory() ? filesAt(path) : [path];
  });
}
export function sha256(path: string) { return createHash('sha256').update(readFileSync(path)).digest('hex'); }
export function sourceFingerprint(scope: Scope, root = process.cwd()) {
  return createHash('sha256').update(scopeInputs(scope, root).map(path => `${path}\0${sha256(join(root, path))}\n`).join('')).digest('hex');
}
export function fingerprints(root = process.cwd()): Record<Scope, string> {
  return Object.fromEntries(SCOPES.map(scope => [scope, sourceFingerprint(scope, root)])) as Record<Scope, string>;
}
export function scopeForSnapshot(snapshot: string): Scope {
  const match = /^(desktop|mobile)\/([a-z0-9-]+)\.png$/.exec(snapshot);
  if (!match) throw new Error(`Invalid PNG identity: ${snapshot}`);
  const name = match[2];
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
export function makeRecord(snapshot: string, fingerprint: string, png: string, sourceSha: string, runUrl: string): BaselineRecord {
  return { schema: 2, snapshot, scope: scopeForSnapshot(snapshot), sourceFingerprint: fingerprint, pngSha256: png,
    sourceSha, runUrl, image: PLAYWRIGHT_IMAGE, playwright: '1.64.0', font: '@fontsource/inter@5.3.0', rendering: RENDERING };
}
export function assertBaselineSet(pngs: Record<string, string>, records: Record<string, unknown>, inputs: Record<Scope, string>): BaselineRecord[] {
  const pngPaths = Object.keys(pngs).sort();
  const recordPaths = Object.keys(records).sort();
  if (JSON.stringify(pngPaths) !== JSON.stringify(recordPaths)) throw new Error('Missing or orphan PNG provenance record');
  return pngPaths.map(snapshot => {
    const value = records[snapshot];
    if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid PNG provenance record');
    const record = value as BaselineRecord;
    if (record.schema !== 2 || record.snapshot !== snapshot || record.scope !== scopeForSnapshot(snapshot)) throw new Error(`Invalid PNG scope or schema: ${snapshot}`);
    if (typeof record.sourceSha !== 'string' || !/^[0-9a-f]{40}$/.test(record.sourceSha)) throw new Error('Invalid producer SHA');
    runId(record.runUrl);
    if (record.image !== PLAYWRIGHT_IMAGE || record.playwright !== '1.64.0' || record.font !== '@fontsource/inter@5.3.0' ||
        JSON.stringify(record.rendering) !== JSON.stringify(RENDERING)) throw new Error(`Unpinned PNG rendering: ${snapshot}`);
    if (!/^[0-9a-f]{64}$/.test(record.sourceFingerprint) || record.sourceFingerprint !== inputs[record.scope]) throw new Error(`Golden inputs changed for ${record.scope}: ${snapshot}`);
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
  return assertBaselineSet(pngHashes(root), readRecords(root), fingerprints(root));
}
