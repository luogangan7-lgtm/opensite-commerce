/**
 * Owns re-run attempt selection for the golden artifact verifier.
 * Invariant: for one run and one lane (job name), the latest attempt GitHub
 * reports as a successful job is the only authoritative evidence. Its exact
 * per-attempt artifact must exist; a missing newer artifact fails closed and an
 * older attempt is never substituted.
 * Depends on: fetch performed by the caller (no imports beyond the runtime).
 * Called by: scripts/frontend/verify-baseline-artifact.ts and
 * rerun-attempts.test.ts. scripts/rerun_attempts.py implements the identical
 * rule for the landing gate; both suites replay tests/fixtures/rerun-attempts.json.
 * Job listings must use `filter=all`: the default filter only shows the latest
 * execution, so a partial re-run can hide an earlier successful producer.
 */

export type RunJob = {
  name?: unknown;
  status?: unknown;
  conclusion?: unknown;
  run_attempt?: unknown;
};
export type RunArtifact = {
  name?: string;
  expired?: boolean;
  id?: number;
  size_in_bytes?: number;
  digest?: string;
};
export type GetJson = (path: string) => Promise<unknown>;

const PAGE_SIZE = 100;
const MAX_PAGES = 100;

/** Normalize a GitHub `run_attempt` to a positive int, or fail closed. */
export function attemptNumber(value: unknown): number {
  const text = value === null || value === undefined ? '' : String(value).trim();
  if (!/^[0-9]+$/.test(text) || Number(text) < 1) throw new Error(`run_attempt ${JSON.stringify(value)} is not a positive integer`);
  return Number(text);
}

/** Ascending attempts where `jobName` completed successfully. */
export function successfulAttempts(jobs: readonly RunJob[], jobName: string): number[] {
  const attempts = new Set<number>();
  for (const job of jobs) {
    if (job?.name !== jobName) continue;
    // A malformed attempt on a matching job is a broken listing, not a reason
    // to skip the lane, so it fails closed here.
    const attempt = attemptNumber(job.run_attempt);
    if (job.status === 'completed' && job.conclusion === 'success') attempts.add(attempt);
  }
  return [...attempts].sort((left, right) => left - right);
}

/** The latest successful attempt, which must itself have evidence. */
export function selectLatestAttempt(successful: readonly number[], evidence: readonly number[]): number {
  const attempts = successful.map(attemptNumber);
  if (attempts.length === 0) throw new Error('no successful attempt');
  const latest = Math.max(...attempts);
  const available = [...new Set(evidence.map(attemptNumber))].sort((left, right) => left - right);
  if (!available.includes(latest)) {
    throw new Error(`latest successful attempt ${latest} has no receipt/artifact evidence (available attempt(s): ${available.join(', ') || 'none'}); an older attempt is never accepted`);
  }
  return latest;
}

/** Latest successful attempt for one job name (evidence is the job itself). */
export function latestSuccessfulAttempt(jobs: readonly RunJob[], jobName: string): number {
  const attempts = successfulAttempts(jobs, jobName);
  return selectLatestAttempt(attempts, attempts);
}

/** Follow `page` until the response says the listing is complete. */
async function readPages(getJson: GetJson, path: string, key: 'jobs' | 'artifacts'): Promise<unknown[]> {
  const items: unknown[] = [];
  for (let page = 1; ; page += 1) {
    if (page > MAX_PAGES) throw new Error(`GitHub ${key} pagination exceeded ${MAX_PAGES} pages`);
    const payload = await getJson(`${path}&page=${page}`);
    if (payload === null || typeof payload !== 'object' || !Array.isArray((payload as Record<string, unknown>)[key])) {
      throw new Error(`GitHub ${key} page ${page} is malformed`);
    }
    const record = payload as Record<string, unknown>;
    const pageItems = record[key] as unknown[];
    items.push(...pageItems);
    const total = record.total_count;
    if (typeof total === 'number' ? items.length >= total : pageItems.length < PAGE_SIZE) return items;
  }
}

/** Every job of this run across every attempt, `filter=all`, paginated. */
export async function listRunJobs(getJson: GetJson, runId: string | number): Promise<RunJob[]> {
  const jobs = await readPages(getJson, `actions/runs/${runId}/jobs?filter=all&per_page=${PAGE_SIZE}`, 'jobs');
  if (jobs.length === 0) throw new Error(`GitHub returned no job results for run ${runId}`);
  return jobs as RunJob[];
}

/** Artifacts matching one exact name, server-side filtered and paginated. */
export async function listRunArtifacts(getJson: GetJson, runId: string | number, name: string): Promise<RunArtifact[]> {
  const query = `actions/runs/${runId}/artifacts?name=${encodeURIComponent(name)}&per_page=${PAGE_SIZE}`;
  return (await readPages(getJson, query, 'artifacts')) as RunArtifact[];
}

/** Exactly one non-expired artifact for the authoritative attempt, or fail closed. */
export function requireAttemptArtifact(artifacts: readonly RunArtifact[], name: string): RunArtifact {
  const candidates = artifacts.filter(artifact => artifact.name === name && artifact.expired !== true);
  if (candidates.length !== 1) throw new Error(`Golden artifact ${name} is missing, expired or ambiguous`);
  return candidates[0];
}
