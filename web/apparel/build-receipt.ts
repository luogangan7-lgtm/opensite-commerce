/** CI seals build bytes; packager verifies an independently supplied CI-log digest before any writes.
 * Depends on fs/crypto only; consumed by CI and copy-images. Receipt is excluded from its own file inventory.
 */
import { readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
const hash = (data: string | Uint8Array) => createHash('sha256').update(data).digest('hex');
function inventory(root: string, prefix = ''): Record<string,string> {
 return Object.fromEntries(readdirSync(join(root,prefix), {withFileTypes:true}).sort((a,b)=>a.name.localeCompare(b.name)).flatMap(e => {
  const p = prefix ? `${prefix}/${e.name}` : e.name;
  if (p === 'build-receipt.json') return [];
  if (e.isSymbolicLink()) throw Error('Symlink in build input');
  return e.isDirectory() ? Object.entries(inventory(root,p)) : [[p,hash(readFileSync(join(root,p)))]];
 }));
}
export function buildReceipt(root: string, sha: string, runId: string) {
 if (!/^[a-f0-9]{40}$/.test(sha) || !/^\d+$/.test(runId)) throw Error('CI build identity required');
 const files = inventory(root);
 if (!files['index.html'] || !files['_worker.js/index.js']) throw Error('Incomplete build input');
 return { schema:1, sha, runId, files };
}
export const receiptDigest = (receipt: ReturnType<typeof buildReceipt>) => hash(JSON.stringify(receipt));
export function verifyBuildReceipt(root: string, expectedSha: string, trustedDigest: string) {
 if (!/^[a-f0-9]{64}$/.test(trustedDigest)) throw Error('Independent CI receipt digest required');
 const receipt = JSON.parse(readFileSync(join(root,'build-receipt.json'),'utf8'));
 if (receiptDigest(receipt) !== trustedDigest || receipt.sha !== expectedSha || receipt.schema !== 1) throw Error('Build identity mismatch');
 const actual = buildReceipt(root,expectedSha,receipt.runId);
 if (JSON.stringify(actual.files) !== JSON.stringify(receipt.files)) throw Error('Build bytes changed');
 return receipt;
}
if (import.meta.main) {
 const root=process.argv[2] || 'web/apparel/dist';
 const receipt=buildReceipt(root,execFileSync('git',['rev-parse','HEAD'],{encoding:'utf8'}).trim(),process.env.GITHUB_RUN_ID || '');
 writeFileSync(join(root,'build-receipt.json'),JSON.stringify(receipt));
 console.log(`BUILD_RECEIPT_SHA256=${receiptDigest(receipt)} run=${receipt.runId} sha=${receipt.sha}`);
}
