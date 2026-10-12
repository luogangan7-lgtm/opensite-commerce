/** Verifies gold bytes against the actual GitHub Actions artifact; CI requires read-only Actions token; no locally authored gold is trusted. */
import { createHash } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { BASELINES, sha256 } from './baseline-provenance';

const repository = 'luogangan7-lgtm/opensite-commerce';
const provenance = JSON.parse(readFileSync(`${BASELINES}/provenance.json`, 'utf8'));
const match = /^https:\/\/github\.com\/luogangan7-lgtm\/opensite-commerce\/actions\/runs\/(\d+)$/.exec(provenance.runUrl);
if (!match || !process.env.GITHUB_TOKEN) throw new Error('Artifact verification requires run URL and read-only GITHUB_TOKEN');
async function api(path: string, raw = false) {
  const response = await fetch(`https://api.github.com/repos/${repository}/${path}`, {
    headers: { Authorization: `Bearer ${process.env.GITHUB_TOKEN}`, Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28' },
    signal: AbortSignal.timeout(60_000)
  });
  if (!response.ok) throw new Error(`GitHub artifact verification failed with HTTP ${response.status}`);
  return raw ? new Uint8Array(await response.arrayBuffer()) : response.json();
}
const runId = match[1];
const run = await api(`actions/runs/${runId}`);
if (run.head_sha !== provenance.sourceSha || run.event !== 'push' || run.repository.full_name !== repository) throw new Error('Golden source SHA does not match CI run');
const jobs = await api(`actions/runs/${runId}/jobs?per_page=100`);
if (!jobs.jobs.some((job: { name: string; conclusion: string }) => job.name === 'frontend-baselines' && job.conclusion === 'success')) throw new Error('Golden CI producer did not succeed');
const artifacts = await api(`actions/runs/${runId}/artifacts?per_page=100`);
const artifact = artifacts.artifacts.find((item: { name: string; expired: boolean }) => item.name === `frontend-baselines-${runId}` && !item.expired);
if (!artifact || artifact.size_in_bytes > 50_000_000 || !/^sha256:[0-9a-f]{64}$/.test(artifact.digest)) throw new Error('Golden artifact is missing, expired or has no SHA256 digest');
const bytes = await api(`actions/artifacts/${artifact.id}/zip`, true) as Uint8Array;
if (`sha256:${createHash('sha256').update(bytes).digest('hex')}` !== artifact.digest) throw new Error('Downloaded golden artifact digest mismatch');
const directory = mkdtempSync(join(tmpdir(), 'opensite-baseline-'));
try {
  const archive = join(directory, 'gold.zip');
  writeFileSync(archive, bytes);
  const readEntry = (name: string) => {
    const result = Bun.spawnSync(['unzip', '-p', archive, name]);
    if (result.exitCode !== 0) throw new Error('Golden artifact entry is missing');
    return result.stdout;
  };
  if (createHash('sha256').update(readEntry('provenance.json')).digest('hex') !== sha256(`${BASELINES}/provenance.json`)) throw new Error('Committed provenance differs from CI artifact');
  for (const path of Object.keys(provenance.pngHashes)) {
    if (!/^(desktop|mobile)\/[a-z0-9-]+\.png$/.test(path)) throw new Error('Invalid golden PNG path');
    if (createHash('sha256').update(readEntry(path)).digest('hex') !== sha256(`${BASELINES}/${path}`)) throw new Error('Committed PNG differs from CI artifact');
  }
  console.log(`PASS: gold bytes verified against CI run ${runId}, artifact ${artifact.id}, SHA ${run.head_sha}`);
} finally { rmSync(directory, { recursive: true, force: true }); }
