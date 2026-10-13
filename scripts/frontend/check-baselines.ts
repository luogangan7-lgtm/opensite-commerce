/** Rejects stale/missing/tampered per-PNG records; CI calls this before artifact authority and visual comparisons. */
import { checkedRecords } from './baseline-provenance';
try {
  const records = checkedRecords();
  if (records.length === 0) throw new Error('Missing all PNG provenance records');
  console.log(`PASS: ${records.length} per-PNG metadata, scoped input fingerprints and hashes; artifact authority checked separately`);
} catch (error) {
  console.error(`FAIL: ${error instanceof Error ? error.message : 'Invalid baseline records'}; request an explicit CI baseline update`);
  process.exit(1);
}
