/**
 * Verifies goldens whose PNG or record bytes changed against the trusted base (HEAD^1: the PR base for
 * pull_request merge commits; github.event.before for pushes, all goldens if unusable) against their CI artifact, the
 * producer workflow and the writer inputs at the producer SHA; CI uses read-only Actions access.
 * Invariant (C-08d): unchanged goldens inherit the provenance verified when they entered main, so neither
 * the 90-day artifact retention nor unrelated input changes can fail a PR; committed hashes are the anchor.
 */
import { createHash } from 'node:crypto';
import { existsSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { BASELINES, REPOSITORY, assertRunAuthority, checkedRecords, goldenChangeSet, recordPath, runId, sha256 } from './baseline-provenance';
import { latestSuccessfulAttempt, listRunArtifacts, listRunJobs, requireAttemptArtifact } from './rerun-attempts';
import { WRITER_INPUTS, assertNoBunEnvFiles, assertProducerWorkflow, assertWriterInputs, blobSha } from './workflow-provenance';

const all = checkedRecords();
if (all.length === 0) throw new Error('No baseline provenance records');
const base = process.env.OPENSITE_GOLDEN_BASE ?? '';
const changed = goldenChangeSet(base);
const records = changed === 'all' ? all : all.filter(record => changed.has(record.snapshot));
if (changed === 'all') console.log(`Golden base ${base || '(none)'} is zero, unknown or not an ancestor; verifying all ${all.length} goldens`);
if (records.length === 0) {
  console.log(`PASS: no golden PNG/record changed against ${base}; ${all.length} committed hashes keep their verified base provenance`);
  process.exit(0);
}
if (!process.env.GITHUB_TOKEN) throw new Error('Artifact verification requires read-only GITHUB_TOKEN');
assertNoBunEnvFiles(readdirSync('.'));
const candidateWriter = Object.fromEntries(WRITER_INPUTS.map(file => [file, blobSha(existsSync(file) ? readFileSync(file) : null)]));
async function api(path: string, raw = false, missing = false) {
  const response = await fetch(`https://api.github.com/repos/${REPOSITORY}/${path}`, {
    headers: { Authorization: `Bearer ${process.env.GITHUB_TOKEN}`, Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28' },
    signal: AbortSignal.timeout(60_000)
  });
  if (missing && response.status === 404) return null;
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
    const atSource: Record<string, string | null> = {};
    for (const file of WRITER_INPUTS) atSource[file] = (await api(`contents/${file}?ref=${run.head_sha}`, false, true))?.sha ?? null;
    assertWriterInputs(atSource, candidateWriter);
    assertNoBunEnvFiles((await api(`contents?ref=${run.head_sha}`) as { name: string }[]).map(entry => entry.name));
    // `filter=all` is required: the default listing only shows the latest
    // execution, so a partial re-run can hide the earlier successful producer.
    // Use the original attempt of the latest successful execution (copies in
    // partial re-runs keep its timestamps). Real re-executions need new artifacts.
    const attempt = latestSuccessfulAttempt(await listRunJobs(api, id), 'frontend-baselines');
    const artifactName = `frontend-baselines-${id}-${attempt}`;
    // A full re-run uploads one immutable artifact per attempt; filter by the
    // exact name server-side and follow pages so more than 100 older artifacts
    // cannot hide this attempt's evidence.
    const artifacts = await listRunArtifacts(api, id, artifactName);
    const artifact = requireAttemptArtifact(artifacts, artifactName);
    const artifactId = artifact.id;
    const digest = artifact.digest;
    const size = artifact.size_in_bytes;
    if (typeof artifactId !== 'number' || typeof size !== 'number' || size <= 0 || size > 50_000_000 || typeof digest !== 'string' || !/^sha256:[0-9a-f]{64}$/.test(digest)) throw new Error('Golden artifact is missing, expired, ambiguous or has no SHA256 digest');
    const bytes = await api(`actions/artifacts/${artifactId}/zip`, true) as Uint8Array;
    if (`sha256:${createHash('sha256').update(bytes).digest('hex')}` !== digest) throw new Error('Downloaded golden artifact digest mismatch');
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
    console.log(`PASS: ${entries.length} PNG/record pairs verified against CI run ${id}, artifact ${artifactId}, SHA ${run.head_sha}`);
  }
} finally { rmSync(directory, { recursive: true, force: true }); }
