/** Writes the candidate comparison receipt (CI artifact + job summary only, never committed); called by the frontend job.
 * Invariant: the receipt is evidence of one comparison run; it never feeds golden provenance or blocks on fingerprints. */
import { appendFileSync, writeFileSync } from 'node:fs';
import { fingerprints, pngHashes } from './baseline-provenance';

let inputs: Record<string, string> | { error: string };
try { inputs = fingerprints(); } catch (error) { inputs = { error: error instanceof Error ? error.message : String(error) }; }
const receipt = {
  schema: 1, repository: process.env.GITHUB_REPOSITORY, runId: process.env.GITHUB_RUN_ID,
  runAttempt: process.env.GITHUB_RUN_ATTEMPT,
  candidateSha: process.env.GITHUB_SHA, headSha: process.env.CANDIDATE_HEAD_SHA,
  result: process.env.VISUAL_OUTCOME ?? 'unknown', inputFingerprints: inputs, goldenPngSha256: pngHashes(),
};
writeFileSync('comparison-receipt.json', JSON.stringify(receipt, null, 2) + '\n');
if (process.env.GITHUB_STEP_SUMMARY) appendFileSync(process.env.GITHUB_STEP_SUMMARY,
  `Visual comparison **${receipt.result}** for ${receipt.headSha} against ${Object.keys(receipt.goldenPngSha256).length} committed goldens (receipt artifact: frontend-comparison-${receipt.runId}-${receipt.runAttempt}).\n`);
console.log(`Comparison receipt: ${receipt.result}, ${Object.keys(receipt.goldenPngSha256).length} goldens`);
