/** Produces golden PNGs only in the explicit GitHub Actions container job; developers download its artifact. */
import { existsSync, rmSync, writeFileSync } from 'node:fs';
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
const selected = SCOPES.filter(scope => {
  const pngs = Object.fromEntries(Object.entries(before).filter(([path]) => scopeForSnapshot(path) === scope));
  const subset = Object.fromEntries(Object.entries(records).filter(([path]) => scopeForSnapshot(path) === scope));
  if (legacy || requested.includes(scope) || Object.keys(pngs).length === 0) return true;
  try { assertBaselineSet(pngs, subset, inputs); return false; } catch { return true; }
});
if (selected.length === 0) {
  console.log('PASS: no affected scopes; preserved every PNG and provenance byte');
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
const result = Bun.spawnSync(['bun', 'x', '--no-install', 'playwright', 'test', ...selected.map(scope => SPECS[scope]), '--update-snapshots'], { stdout: 'inherit', stderr: 'inherit' });
if (result.exitCode !== 0) process.exit(result.exitCode);
for (const [snapshot, png] of Object.entries(pngHashes())) {
  const scope = scopeForSnapshot(snapshot);
  if (!selected.includes(scope)) {
    if (before[snapshot] !== png) throw new Error(`Unselected PNG changed: ${snapshot}`);
    continue;
  }
  const record = makeRecord(snapshot, inputs[scope], png, process.env.GITHUB_SHA!, `https://github.com/${REPOSITORY}/actions/runs/${process.env.GITHUB_RUN_ID}`);
  writeFileSync(`${BASELINES}/${recordPath(snapshot)}`, JSON.stringify(record, null, 2) + '\n');
}
if (legacy) rmSync(`${BASELINES}/provenance.json`);
const afterRecords = readRecords();
assertBaselineSet(pngHashes(), afterRecords, inputs);
for (const [snapshot, record] of Object.entries(records)) {
  if (!selected.includes(scopeForSnapshot(snapshot)) && (JSON.stringify(afterRecords[snapshot]) !== JSON.stringify(record) || sha256(`${BASELINES}/${recordPath(snapshot)}`) !== recordHashes[snapshot])) throw new Error(`Unselected provenance changed: ${snapshot}`);
}
// Raw bytes are never rewritten for unselected records; this read ensures they remain present.
for (const scope of selected) if (!Object.keys(afterRecords).some(path => scopeForSnapshot(path) === scope)) throw new Error(`Selected scope produced no PNGs: ${scope}`);
console.log(`PASS: regenerated ${selected.length} affected scopes, preserving all other CI records`);
