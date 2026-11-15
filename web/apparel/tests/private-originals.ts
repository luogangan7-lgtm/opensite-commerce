/** Test-only private input boundary: opt in with APPAREL_SOURCE_DIR, then fail on damaged inputs.
 * Depends on Node fs/crypto/path; consumed by imagery.test.ts. Directory presence grants no opt-in.
 */
import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { resolve, join } from 'node:path';
const digest = (bytes: Uint8Array) => createHash('sha256').update(bytes).digest('hex');
export function privateSourceRoot(env: { APPAREL_SOURCE_DIR?: string }): string | undefined {
  if (env.APPAREL_SOURCE_DIR === undefined) return undefined;
  if (!env.APPAREL_SOURCE_DIR.trim()) throw Error('APPAREL_SOURCE_DIR must explicitly name the private bundle');
  return resolve(env.APPAREL_SOURCE_DIR);
}
export function assertPrivateOriginalInputs(root: string, images: readonly { source: string; acceptance: string; sha256: string }[], catalogSha: string): void {
  // No exists/ENOENT fallback: declaring a bundle makes every expected input mandatory.
  const catalog = readFileSync(resolve(root, '../../docs/apparel-b2b/catalog.json'));
  if (digest(catalog) !== catalogSha) throw Error('Private catalog SHA mismatch');
  for (const image of images) {
    if (digest(readFileSync(join(root, image.source))) !== image.sha256) throw Error('Private source SHA mismatch');
    readFileSync(join(root, image.acceptance));
  }
}
