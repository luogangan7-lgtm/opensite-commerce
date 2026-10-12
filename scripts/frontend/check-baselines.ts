/** Rejects stale or modified golden inputs before comparisons; CI artifact/run must be independently inspected on update. */
import { readFileSync } from 'node:fs';
import { BASELINES, PLAYWRIGHT_IMAGE, pngHashes, sourceFingerprint } from './baseline-provenance';
const provenance = JSON.parse(readFileSync(`${BASELINES}/provenance.json`, 'utf8'));
if (provenance.image !== PLAYWRIGHT_IMAGE || provenance.sourceFingerprint !== sourceFingerprint() ||
    !/^https:\/\/github\.com\/luogangan7-lgtm\/opensite-commerce\/actions\/runs\/\d+$/.test(provenance.runUrl) ||
    !/^[0-9a-f]{40}$/.test(provenance.sourceSha) ||
    JSON.stringify(provenance.pngHashes) !== JSON.stringify(pngHashes())) {
  console.error('FAIL: golden inputs changed; request an explicit CI baseline update');
  process.exit(1);
}
console.log('PASS: baseline metadata consistency and source/PNG hashes (artifact authority checked separately)');
