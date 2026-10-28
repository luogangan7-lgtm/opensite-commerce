/** Rejects missing/orphan/tampered per-PNG records and unpinned renderers; CI calls this before artifact authority
 * and the candidate comparison. It does not compare input fingerprints (C-08d): pixels decide staleness. */
import { checkedRecords, pngHashes } from './baseline-provenance';
import { assertDistinctHistoryPngs } from './real-visual-manifest';
try {
  const records = checkedRecords();
  assertDistinctHistoryPngs(pngHashes());
  if (records.length === 0) throw new Error('Missing all PNG provenance records');
  console.log(`PASS: ${records.length} per-PNG provenance records, pins and hashes; changed goldens get artifact authority next`);
} catch (error) {
  console.error(`FAIL: ${error instanceof Error ? error.message : 'Invalid baseline records'}; request an explicit CI baseline update`);
  process.exit(1);
}
