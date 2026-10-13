/** Verifies each PNG/record against its originating CI artifact; CI uses read-only Actions access; mixed runs are cached once. */
import { createHash } from 'node:crypto';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { BASELINES, REPOSITORY, assertRunAuthority, checkedRecords, recordPath, runId, sha256 } from './baseline-provenance';
import { assertProducerWorkflow } from './workflow-provenance';

if (!process.env.GITHUB_TOKEN) throw new Error('Artifact verification requires read-only GITHUB_TOKEN');
const records = checkedRecords();
if (records.length === 0) throw new Error('No baseline provenance records');
async function api(path: string, raw = false) {
  const response = await fetch(`https://api.github.com/repos/${REPOSITORY}/${path}`, {
    headers: { Authorization: `Bearer ${process.env.GITHUB_TOKEN}`, Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28' },
    signal: AbortSignal.timeout(60_000)
  });
  if (!response.ok) throw new Error(`GitHub artifact verification failed with HTTP ${response.status}`);
  return raw ? new Uint8Array(await response.arrayBuffer()) : response.json();
}
const directory = mkdtempSync(join(tmpdir(), 'opensite-baseline-'));
try {
  const groups = new Map<string, typeof records>();
  for (const record of records) {
    const id = runId(record.runUrl);
    const entries = groups.get(id) ?? [];
    entries.push(record); groups.set(id, entries);
  }
  for (const [id, entries] of groups) {
    const run = await api(`actions/runs/${id}`);
    for (const record of entries) assertRunAuthority(record, run);
    const workflow = await api(`contents/.github/workflows/checks.yml?ref=${run.head_sha}`);
    if (workflow.encoding !== 'base64') throw new Error('Golden producer workflow source unavailable');
    assertProducerWorkflow(run.path, Buffer.from(workflow.content, 'base64').toString('utf8'));
    const jobs = await api(`actions/runs/${id}/jobs?per_page=100`);
    if (!jobs.jobs.some((job: { name: string; conclusion: string }) => job.name === 'frontend-baselines' && job.conclusion === 'success')) throw new Error('Golden CI producer did not succeed');
    const artifacts = await api(`actions/runs/${id}/artifacts?per_page=100`);
    const candidates = artifacts.artifacts.filter((item: { name: string; expired: boolean }) => item.name === `frontend-baselines-${id}` && !item.expired);
    const artifact = candidates[0];
    if (candidates.length !== 1 || artifact.size_in_bytes > 50_000_000 || !/^sha256:[0-9a-f]{64}$/.test(artifact.digest)) throw new Error('Golden artifact is missing, expired, ambiguous or has no SHA256 digest');
    const bytes = await api(`actions/artifacts/${artifact.id}/zip`, true) as Uint8Array;
    if (`sha256:${createHash('sha256').update(bytes).digest('hex')}` !== artifact.digest) throw new Error('Downloaded golden artifact digest mismatch');
    const archive = join(directory, `${id}.zip`);
    writeFileSync(archive, bytes);
    const list = Bun.spawnSync(['unzip', '-Z1', archive]);
    if (list.exitCode !== 0) throw new Error('Cannot enumerate golden artifact');
    const paths = new TextDecoder().decode(list.stdout).split('\n').filter(Boolean);
    if (paths.length !== new Set(paths).size || paths.some(path => !/^(?:desktop|mobile)\/(?:[a-z0-9-]+\.png(?:\.provenance\.json)?)?$/.test(path))) throw new Error('Unsafe or duplicate golden artifact entry');
    const readEntry = (name: string) => {
      if (!paths.includes(name)) throw new Error(`Golden artifact entry is missing: ${name}`);
      const result = Bun.spawnSync(['unzip', '-p', archive, name]);
      if (result.exitCode !== 0) throw new Error('Golden artifact entry is unreadable');
      return result.stdout;
    };
    for (const record of entries) {
      for (const path of [record.snapshot, recordPath(record.snapshot)]) {
        if (createHash('sha256').update(readEntry(path)).digest('hex') !== sha256(`${BASELINES}/${path}`)) throw new Error(`Committed PNG/provenance differs from CI artifact: ${path}`);
      }
    }
    console.log(`PASS: ${entries.length} PNG/record pairs verified against CI run ${id}, artifact ${artifact.id}, SHA ${run.head_sha}`);
  }
} finally { rmSync(directory, { recursive: true, force: true }); }
