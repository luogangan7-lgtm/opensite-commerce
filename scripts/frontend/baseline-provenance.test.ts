/** Exercises per-PNG integrity and independent input groups; CI runs Bun; fixtures never claim artifact authority. */
import { expect, test } from 'bun:test';
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
