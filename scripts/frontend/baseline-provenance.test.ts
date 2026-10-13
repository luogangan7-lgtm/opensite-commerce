/** Exercises per-PNG integrity and independent input groups; CI runs Bun; fixtures never claim artifact authority. */
import { expect, test } from 'bun:test';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import * as implementation from './baseline-provenance';

type Scope = 'gallery' | 'admin' | 'storefront-published' | 'storefront-commerce';
type RecordValue = { schema: number; snapshot: string; scope: Scope; sourceFingerprint: string; pngSha256: string; runUrl: string; sourceSha: string; image: string; rendering: unknown };
type API = {
  makeRecord(snapshot: string, fingerprint: string, png: string, sourceSha: string, runUrl: string): RecordValue;
  assertBaselineSet(pngs: Record<string, string>, records: Record<string, unknown>): RecordValue[];
  changedSnapshots(paths: string[]): Set<string>;
  scopeForSnapshot(snapshot: string): Scope;
  assertRunAuthority(record: RecordValue, run: unknown): void;
};
const api = implementation as unknown as API;
const hashes = { gallery: 'a'.repeat(64), admin: 'b'.repeat(64), 'storefront-published': 'c'.repeat(64), 'storefront-commerce': 'd'.repeat(64) };
const sha = '1'.repeat(40);
const runUrl = 'https://github.com/luogangan7-lgtm/opensite-commerce/actions/runs/123';
const png = 'e'.repeat(64);
const golden = () => api.makeRecord('desktop/admin-orders.png', hashes.admin, png, sha, runUrl);

test('each PNG has exactly one valid independent provenance record', () => {
  const record = golden();
  expect(record.schema).toBe(2);
  expect(api.assertBaselineSet({ [record.snapshot]: png }, { [record.snapshot]: record })).toEqual([record]);
});
test('tampering with a single PNG is rejected', () => {
  const record = golden();
  expect(() => api.assertBaselineSet({ [record.snapshot]: 'f'.repeat(64) }, { [record.snapshot]: record })).toThrow(/PNG/);
});
test('missing and orphan records are rejected', () => {
  const record = golden();
  expect(() => api.assertBaselineSet({ [record.snapshot]: png }, {})).toThrow(/record/);
  expect(() => api.assertBaselineSet({}, { [record.snapshot]: record })).toThrow(/record/);
});
test('an author cannot choose another scope or a malformed input fingerprint', () => {
  const record = golden();
  expect(() => api.assertBaselineSet({ [record.snapshot]: png }, { [record.snapshot]: { ...record, scope: 'gallery' } })).toThrow(/scope/);
  expect(() => api.assertBaselineSet({ [record.snapshot]: png }, { [record.snapshot]: { ...record, sourceFingerprint: 'x' } })).toThrow(/fingerprint/);
});
test('C-08d: changed renderer inputs with identical pixels need no new PNG or record', () => {
  // The record keeps the producer-time fingerprint as evidence; the candidate comparison, not equality, decides staleness.
  const record = golden();
  const current = { ...hashes, admin: 'f'.repeat(64) };
  expect(current.admin).not.toBe(record.sourceFingerprint);
  expect(api.assertBaselineSet({ [record.snapshot]: png }, { [record.snapshot]: record })).toEqual([record]);
});
test('C-08d: only PNGs or records changed against the trusted base need their producer artifact', () => {
  expect([...api.changedSnapshots([
    'tests/frontend/baselines/desktop/admin-orders.png',
    'tests/frontend/baselines/mobile/gallery.png.provenance.json',
    'api/openapi.yaml', 'tests/frontend/admin.spec.ts',
  ])].sort()).toEqual(['desktop/admin-orders.png', 'mobile/gallery.png']);
});
test('forged URLs, source SHA and a real but different producer run are rejected', () => {
  const record = golden();
  const run = { id: 123, head_sha: sha, event: 'push', path: '.github/workflows/checks.yml', repository: { full_name: 'luogangan7-lgtm/opensite-commerce' } };
  expect(() => api.assertRunAuthority(record, run)).not.toThrow();
  expect(() => api.assertRunAuthority({ ...record, runUrl: runUrl.replace('123', '456') }, run)).toThrow(/run/);
  expect(() => api.assertRunAuthority({ ...record, sourceSha: '2'.repeat(40) }, run)).toThrow(/SHA/);
  expect(() => api.assertBaselineSet({ [record.snapshot]: png }, { [record.snapshot]: { ...record, runUrl: 'https://attacker.invalid/actions/runs/123' } })).toThrow(/run/);
});
test('two branches changing distinct scopes merge their records without a global fingerprint', () => {
  const admin = golden();
  const commerce = api.makeRecord('mobile/storefront-commerce-cart.png', hashes['storefront-commerce'], png, sha, runUrl);
  const base = { [admin.snapshot]: admin, [commerce.snapshot]: commerce };
  const branchA = { ...base, [admin.snapshot]: { ...admin, sourceFingerprint: '5'.repeat(64), runUrl: runUrl.replace('123', '124') } };
  const branchB = { ...base, [commerce.snapshot]: { ...commerce, sourceFingerprint: '6'.repeat(64), runUrl: runUrl.replace('123', '125') } };
  const merged = { [admin.snapshot]: branchA[admin.snapshot], [commerce.snapshot]: branchB[commerce.snapshot] };
  expect(api.assertBaselineSet({ [admin.snapshot]: png, [commerce.snapshot]: png }, merged)).toHaveLength(2);
  expect(JSON.stringify(branchA[commerce.snapshot])).toBe(JSON.stringify(base[commerce.snapshot]));
  expect(JSON.stringify(branchB[admin.snapshot])).toBe(JSON.stringify(base[admin.snapshot]));
});
test('unknown snapshots and traversal cannot obtain an arbitrary input scope', () => {
  expect(api.scopeForSnapshot('desktop/gallery.png')).toBe('gallery');
  expect(api.scopeForSnapshot('mobile/storefront-home.png')).toBe('storefront-published');
  expect(() => api.scopeForSnapshot('desktop/unknown.png')).toThrow();
  expect(() => api.scopeForSnapshot('../desktop/admin-orders.png')).toThrow();
});
test('deleting a PNG and its record together cannot shrink the producer-observed scope inventory', () => {
  const record = { ...golden(), scopeSnapshots: ['desktop/admin-orders.png', 'mobile/admin-orders.png'] };
  expect(() => api.assertBaselineSet({ [record.snapshot]: png }, { [record.snapshot]: record })).toThrow(/inventory/);
});
test('C-08d/INF-MIG: renumbering or landing a migration without content change keeps the fingerprint', () => {
  const { fingerprintOf } = implementation as unknown as { fingerprintOf(entries: [string, string][]): string };
  const content = '9'.repeat(64);
  const base: [string, string][] = [['web/admin/index.html', 'a'.repeat(64)], ['migrations/001_init.sql', '1'.repeat(64)]];
  const landed = fingerprintOf([...base, ['migrations/034_k7q2_orders.sql', content]]);
  expect(fingerprintOf([...base, ['migrations/035_k7q2_orders.sql', content]])).toBe(landed);
  expect(fingerprintOf([...base, ['migrations/pending/k7q2_orders.sql', content]])).toBe(landed);
  expect(fingerprintOf([...base, ['migrations/034_k7q2_orders.sql', '8'.repeat(64)]])).not.toBe(landed);
  const stamped = fingerprintOf([...base, ['migrations/pending/20261013_orders.sql', content]]);
  expect(fingerprintOf([...base, ['migrations/041_20261013_orders.sql', content]])).toBe(stamped);
  expect(fingerprintOf([...base, ['migrations/034_k7q2_other.sql', content]])).not.toBe(landed);
});

// Independent review P1 on #103: a push may carry several commits; HEAD^1 only covers the last one.
type ChangeSet = (base: string, cwd?: string) => Set<string> | 'all';
const goldenChangeSet = () => (implementation as unknown as { goldenChangeSet: ChangeSet }).goldenChangeSet;
function repository() {
  const root = mkdtempSync(join(tmpdir(), 'c08d-push-base-'));
  const git = (...args: string[]) => {
    const result = Bun.spawnSync(['git', '-c', 'user.name=ci', '-c', 'user.email=ci@example.invalid', '-c', 'commit.gpgsign=false', ...args], { cwd: root });
    if (result.exitCode !== 0) throw new Error(new TextDecoder().decode(result.stderr));
    return new TextDecoder().decode(result.stdout).trim();
  };
  const write = (path: string, text: string) => { mkdirSync(join(root, path, '..'), { recursive: true }); writeFileSync(join(root, path), text); };
  const commit = (message: string) => { git('add', '-A'); git('commit', '-q', '--allow-empty', '-m', message); return git('rev-parse', 'HEAD'); };
  git('init', '-q');
  write('tests/frontend/baselines/desktop/admin-orders.png', 'v1');
  return { root, git, write, commit, base: commit('base') };
}
test('push base: a multi-commit push covers a golden changed by an earlier commit', () => {
  const repo = repository();
  try {
    repo.write('tests/frontend/baselines/desktop/admin-orders.png', 'forged');
    repo.commit('earlier commit replaces a golden');
    repo.write('README.md', 'later unrelated commit');
    repo.commit('later commit');
    expect(goldenChangeSet()('HEAD^1', repo.root)).toEqual(new Set());
    expect(goldenChangeSet()(repo.base, repo.root)).toEqual(new Set(['desktop/admin-orders.png']));
  } finally { rmSync(repo.root, { recursive: true, force: true }); }
});
test('push base: zero, empty, unknown or non-ancestor (force-push) base verifies every golden', () => {
  const repo = repository();
  try {
    repo.git('checkout', '-q', '--orphan', 'rewritten');
    const unrelated = repo.commit('force-pushed history');
    repo.git('checkout', '-q', '-');
    for (const base of ['0'.repeat(40), '', 'f'.repeat(40), unrelated]) expect(goldenChangeSet()(base, repo.root)).toBe('all');
    expect(goldenChangeSet()(repo.base, repo.root)).toEqual(new Set());
  } finally { rmSync(repo.root, { recursive: true, force: true }); }
});
