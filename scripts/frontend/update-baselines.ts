/** Produces golden PNGs only in the explicit GitHub Actions container job; developers download its artifact.
 * C-08d: a scope is re-rendered only when its candidate comparison fails (or it is missing/invalid/legacy/
 * explicitly requested). Input changes with identical pixels keep every PNG and record byte. No repository code
 * runs before this file (workflow-provenance.ts), so the storefront build happens here. */
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { BASELINES, PLAYWRIGHT_IMAGE, REPOSITORY, SCOPES, SPECS, assertBaselineSet, fingerprints, makeRecord, pngHashes, readRecords, recordPath, scopeForSnapshot, sha256, type Scope } from './baseline-provenance';
if (process.env.GITHUB_ACTIONS !== 'true' || process.env.OPENSITE_BASELINE_UPDATE !== '1' ||
    process.env.PLAYWRIGHT_IMAGE !== PLAYWRIGHT_IMAGE || process.platform !== 'linux' || process.arch !== 'x64' ||
    !process.env.GITHUB_RUN_ID || !process.env.GITHUB_SHA || process.env.GITHUB_REPOSITORY !== REPOSITORY) {
  console.error('FAIL: baseline updates require the explicit pinned GitHub Actions container job');
  process.exit(1);
}
const inputs = fingerprints();
const before = pngHashes();
const legacy = existsSync(`${BASELINES}/provenance.json`);
const records = legacy ? {} : readRecords();
const recordHashes = Object.fromEntries(Object.keys(records).map(snapshot => [snapshot, sha256(`${BASELINES}/${recordPath(snapshot)}`)]));
const requested = (process.env.OPENSITE_BASELINE_SCOPES ?? '').split(',').filter(Boolean);
for (const scope of requested) if (!SCOPES.includes(scope as Scope)) throw new Error(`Unknown renewal scope: ${scope}`);
const spawn = (command: string[], env: Record<string, string> = {}) =>
  Bun.spawnSync(command, { stdout: 'inherit', stderr: 'inherit', env: { ...process.env, ...env } }).exitCode;
if (spawn(['bun', 'run', 'build:storefront']) !== 0) throw new Error('Storefront build failed before golden comparison');
const reportDirectory = mkdtempSync(join(tmpdir(), 'opensite-golden-compare-'));
const report = join(reportDirectory, 'report.json');
// Compare every spec against the committed goldens; a failing spec file marks its scope for re-rendering.
const compared = spawn(['bun', 'x', '--no-install', 'playwright', 'test', '--reporter=json'], { PLAYWRIGHT_JSON_OUTPUT_NAME: report });
type Suite = { specs?: { ok: boolean; file: string }[]; suites?: Suite[] };
const parsed = JSON.parse(readFileSync(report, 'utf8')) as Suite & { errors?: unknown[] };
rmSync(reportDirectory, { recursive: true, force: true });
const failing = new Set<Scope>();
const walk = (suite: Suite): void => {
  for (const spec of suite.specs ?? []) if (!spec.ok) {
    const scope = SCOPES.find(item => SPECS[item] === `tests/frontend/${spec.file}`);
    if (!scope) throw new Error(`Failing spec has no golden scope: ${spec.file}`);
    failing.add(scope);
  }
  for (const child of suite.suites ?? []) walk(child);
};
walk(parsed);
if (parsed.errors?.length || compared !== 0 && failing.size === 0) throw new Error('Golden comparison failed outside any spec; refusing to choose scopes');
const selected = SCOPES.filter(scope => {
  const pngs = Object.fromEntries(Object.entries(before).filter(([path]) => scopeForSnapshot(path) === scope));
  const subset = Object.fromEntries(Object.entries(records).filter(([path]) => scopeForSnapshot(path) === scope));
  if (legacy || requested.includes(scope) || Object.keys(pngs).length === 0 || failing.has(scope)) return true;
  // An uploaded record claiming this run must have been rendered here, never prepopulated before the writer.
  if (Object.values(subset).some(value => (value as { runUrl?: string })?.runUrl === `https://github.com/${REPOSITORY}/actions/runs/${process.env.GITHUB_RUN_ID}`)) return true;
  try { assertBaselineSet(pngs, subset); return false; } catch { return true; }
});
if (selected.length === 0) {
  console.log('PASS: every scope matches its committed goldens; preserved every PNG and provenance byte');
  process.exit(0);
}
// Remove selected old snapshots so --update-snapshots cannot bless untouched stale PNGs.
// The real tests must recreate them; all other records and PNGs retain their original run.
for (const snapshot of new Set([...Object.keys(before), ...Object.keys(records)])) {
  if (!selected.includes(scopeForSnapshot(snapshot))) continue;
  rmSync(`${BASELINES}/${snapshot}`, { force: true });
  rmSync(`${BASELINES}/${recordPath(snapshot)}`, { force: true });
}
console.log(`CI baseline generation scopes: ${selected.join(', ')}`);
const updated = spawn(['bun', 'x', '--no-install', 'playwright', 'test', ...selected.map(scope => SPECS[scope]), '--update-snapshots']);
if (updated !== 0) process.exit(updated);
const after = pngHashes();
for (const [snapshot, png] of Object.entries(after)) {
  const scope = scopeForSnapshot(snapshot);
  if (!selected.includes(scope)) {
    if (before[snapshot] !== png) throw new Error(`Unselected PNG changed: ${snapshot}`);
    continue;
  }
  const inventory = Object.keys(after).filter(path => scopeForSnapshot(path) === scope).sort();
  const record = makeRecord(snapshot, inputs[scope], png, process.env.GITHUB_SHA!, `https://github.com/${REPOSITORY}/actions/runs/${process.env.GITHUB_RUN_ID}`, inventory);
  writeFileSync(`${BASELINES}/${recordPath(snapshot)}`, JSON.stringify(record, null, 2) + '\n');
}
if (legacy) rmSync(`${BASELINES}/provenance.json`);
const afterRecords = readRecords();
assertBaselineSet(pngHashes(), afterRecords);
for (const [snapshot, record] of Object.entries(records)) {
  if (!selected.includes(scopeForSnapshot(snapshot)) && (JSON.stringify(afterRecords[snapshot]) !== JSON.stringify(record) || sha256(`${BASELINES}/${recordPath(snapshot)}`) !== recordHashes[snapshot])) throw new Error(`Unselected provenance changed: ${snapshot}`);
}
// Raw bytes are never rewritten for unselected records; this read ensures they remain present.
for (const scope of selected) for (const viewport of ['desktop', 'mobile']) if (!Object.keys(afterRecords).some(path => scopeForSnapshot(path) === scope && path.startsWith(`${viewport}/`))) throw new Error(`Selected scope produced no ${viewport} PNGs: ${scope}`);
console.log(`PASS: regenerated ${selected.length} scopes whose comparison failed or were invalid, preserving all other CI records`);
