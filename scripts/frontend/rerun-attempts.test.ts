/**
 * #426/#419: pins the re-run attempt rule shared with scripts/check_landing.py.
 * The vectors in tests/fixtures/rerun-attempts.json are the cross-language
 * contract; this file replays them against the TypeScript implementation and
 * adds the GitHub listing behaviour that only the producer verifier exercises
 * (filter=all job pages; exact-name artifact pages).
 */
import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { latestSuccessfulAttempt, listRunArtifacts, listRunJobs, requireAttemptArtifact, selectLatestAttempt, successfulAttempts } from './rerun-attempts';
import type { RunJob } from './rerun-attempts';

type Job = RunJob;
type Case = { name: string; jobs?: Job[]; job_pages?: Job[][]; evidence_attempts: number[]; expected: { attempt?: number; error?: string } };
type Vectors = { job_name: string; cases: Case[] };
const vectors = JSON.parse(readFileSync(new URL('../../tests/fixtures/rerun-attempts.json', import.meta.url), 'utf8')) as Vectors;

for (const item of vectors.cases) {
  test(`shared vectors: ${item.name}`, () => {
    const jobs = item.jobs ?? (item.job_pages ?? []).flat();
    const expected = item.expected;
    if (expected.error) {
      expect(() => selectLatestAttempt(successfulAttempts(jobs, vectors.job_name), item.evidence_attempts)).toThrow(expected.error);
    } else {
      expect(selectLatestAttempt(successfulAttempts(jobs, vectors.job_name), item.evidence_attempts)).toBe(expected.attempt!);
      expect(latestSuccessfulAttempt(jobs, vectors.job_name)).toBe(expected.attempt!);
    }
  });
}

test('run jobs are listed with filter=all across every page, not only the latest execution', async () => {
  const requested: string[] = [];
  const pageOne = [
    { name: 'frontend-baselines', run_attempt: 1, status: 'completed', conclusion: 'success' },
    ...Array.from({ length: 99 }, (_, index) => ({ name: `other-${index}`, run_attempt: 1, status: 'completed', conclusion: 'success' }))
  ];
  const pageTwo = [{ name: 'frontend-baselines', run_attempt: 2, status: 'completed', conclusion: 'success' }];
  const pages = [pageOne, pageTwo];
  const api = async (path: string) => {
    requested.push(path);
    const page = Number(new URL(`https://api.github.test/${path}`).searchParams.get('page'));
    return { total_count: 101, jobs: pages[page - 1] };
  };
  const jobs = await listRunJobs(api, '77');
  expect(jobs.length).toBe(101);
  expect(requested[0]).toContain('filter=all');
  expect(requested[0]).toContain('per_page=100');
  expect(requested.some(path => path.includes('page=2'))).toBe(true);
  expect(latestSuccessfulAttempt(jobs, 'frontend-baselines')).toBe(2);
});

test('the exact attempt artifact is found on a later page by server-side name filtering', async () => {
  const name = 'frontend-baselines-77-2';
  const requested: string[] = [];
  const pages = [
    Array.from({ length: 100 }, (_, index) => ({ name: `unrelated-${index}`, expired: false })),
    [{ name, expired: false, size_in_bytes: 1, digest: `sha256:${'a'.repeat(64)}` }]
  ];
  const api = async (path: string) => {
    requested.push(path);
    const url = new URL(`https://api.github.test/${path}`);
    expect(url.searchParams.get('name')).toBe(name);
    const page = Number(url.searchParams.get('page'));
    return { total_count: 101, artifacts: pages[page - 1] };
  };
  const artifacts = await listRunArtifacts(api, '77', name);
  expect(requireAttemptArtifact(artifacts, name).name).toBe(name);
  expect(requested.length).toBe(2);
});

test('missing, expired or duplicate attempt artifacts fail closed', () => {
  const name = 'frontend-baselines-77-2';
  expect(() => requireAttemptArtifact([], name)).toThrow(/missing, expired or ambiguous/);
  expect(() => requireAttemptArtifact([{ name, expired: true }], name)).toThrow(/missing, expired or ambiguous/);
  expect(() => requireAttemptArtifact([{ name, expired: false }, { name, expired: false }], name)).toThrow(/missing, expired or ambiguous/);
});
