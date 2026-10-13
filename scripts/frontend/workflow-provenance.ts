/**
 * Responsibility: decide whether a golden producer run could only have executed the reviewed writer.
 * Dependencies: Bun YAML parser and pinned image constant; called by verify-baseline-artifact.ts and its tests.
 * Invariants (C-08d, threads r23sS / r6FFa): before the writer no step runs repository code or carries
 * injected env/inputs (pinned tool setup only), so the executed writer is exactly the file at the run's
 * source SHA; the verifier then requires those writer inputs to equal the candidate tree. Renderer code the
 * writer later executes cannot forge a passing golden: the candidate zero-diff comparison would fail.
 */
import { createHash } from 'node:crypto';
import { PLAYWRIGHT_IMAGE } from './baseline-provenance';

type Step = Record<string, unknown> & { run?: unknown; uses?: unknown; with?: Record<string, unknown> };
type Job = Record<string, unknown> & { container?: unknown; env?: Record<string, unknown>; services?: Record<string, Record<string, unknown>>; steps?: Step[] };
const WRITER = 'bun run visual:update';
const UPLOAD = 'actions/upload-artifact@b7c566a772e6b6bfb58ed0dc250532a479d7789f';
const POSTGRES_SERVICE = { postgres: {
  image: 'postgres:18.6@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336',
  env: { POSTGRES_DB: 'opensite', POSTGRES_USER: 'postgres', POSTGRES_PASSWORD: 'ci_test_only' },
  options: '--health-cmd "pg_isready -U postgres -d opensite" --health-interval 5s --health-timeout 5s --health-retries 20',
} };
// Allowed pre-writer commands install pinned tools/modules only; lifecycle scripts are disabled.
const PRE_WRITER_RUNS = new Set([
  'git config --global --add safe.directory "$GITHUB_WORKSPACE"',
  'apt-get update -qq && apt-get install -y -qq --no-install-recommends unzip postgresql-client',
  'go mod download',
  'bun install --frozen-lockfile --ignore-scripts',
]);
// Pinned setup actions and the only inputs they may receive (checkout gets none: no other ref/repository).
const PRE_WRITER_USES: Record<string, string[]> = {
  'actions/checkout@8e8c483db84b4bee98b60c0593521ed34d9990e8': [],
  'actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417': ['go-version'],
  'oven-sh/setup-bun@0c5077e51419868618aeaa5fe8019c62421857d6': ['bun-version'],
};
/** Everything Bun reads to start the writer: entry script, its local imports, package/lock/tsconfig; bunfig.toml must be absent. */
export const WRITER_INPUTS = ['package.json', 'bun.lock', 'bunfig.toml', 'tsconfig.json',
  'scripts/frontend/update-baselines.ts', 'scripts/frontend/baseline-provenance.ts', 'scripts/frontend/scoped-inputs.ts'];

const only = (value: unknown, keys: string[]) =>
  !!value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).every(key => keys.includes(key));

export function assertProducerWorkflow(path: string, source: string) {
  if (path !== '.github/workflows/checks.yml') throw new Error('Unexpected golden producer workflow');
  const workflow = Bun.YAML.parse(source) as { jobs?: Record<string, Job> };
  const job = workflow.jobs?.['frontend-baselines'];
  const fail = (reason: string): never => { throw new Error(`Golden producer cannot prove its writer: ${reason}`); };
  if (!job) return fail('producer job missing');
  if (!only(workflow, ['name', 'on', 'true', 'concurrency', 'jobs', 'permissions'])) fail('workflow-level env/defaults');
  if (!only(job, ['if', 'runs-on', 'timeout-minutes', 'permissions', 'container', 'env', 'services', 'steps'])) fail('job keys outside the allowlist');
  if (job['runs-on'] !== 'ubuntu-24.04' || JSON.stringify(job.container) !== JSON.stringify({ image: PLAYWRIGHT_IMAGE, options: '--ipc=host' })) fail('runner/container not pinned');
  const { OPENSITE_BASELINE_SCOPES: scopes, ...env } = job.env ?? {};
  if (scopes !== undefined && !(typeof scopes === 'string' && /^[a-z,-]*$/.test(scopes))) fail('invalid OPENSITE_BASELINE_SCOPES');
  if (JSON.stringify(Object.keys(env).sort()) !== JSON.stringify(['DATABASE_URL', 'GOTOOLCHAIN', 'OPENSITE_BASELINE_UPDATE', 'PLAYWRIGHT_IMAGE']) ||
      env.GOTOOLCHAIN !== 'local' || env.OPENSITE_BASELINE_UPDATE !== '1' || env.PLAYWRIGHT_IMAGE !== PLAYWRIGHT_IMAGE) fail('job env outside the allowlist');
  // The only service is the exact pinned PostgreSQL fixture: no other env, Docker options, entrypoint or healthcheck.
  if (job.services !== undefined && JSON.stringify(job.services) !== JSON.stringify(POSTGRES_SERVICE)) fail('service outside the exact allowlist');
  const steps = job.steps ?? [];
  const writer = steps.findIndex(step => step.run === WRITER);
  if (writer < 0 || steps.filter(step => step.run === WRITER).length !== 1 || !only(steps[writer], ['name', 'run'])) fail('one unmodified writer step required');
  for (const step of steps.slice(0, writer)) {
    const allowed = typeof step.run === 'string'
      ? only(step, ['name', 'run']) && PRE_WRITER_RUNS.has(step.run.trim())
      : typeof step.uses === 'string' && step.uses in PRE_WRITER_USES && only(step, ['name', 'uses', 'with']) &&
        (step.with === undefined || only(step.with, PRE_WRITER_USES[step.uses]!));
    if (!allowed) fail(`pre-writer step outside the allowlist: ${JSON.stringify(step).slice(0, 120)}`);
  }
  const upload = steps[writer + 1];
  if (steps.length !== writer + 2 || !upload || !only(upload, ['name', 'uses', 'with']) || upload.uses !== UPLOAD ||
      JSON.stringify(upload.with) !== JSON.stringify({ name: 'frontend-baselines-${{ github.run_id }}', path: 'tests/frontend/baselines', 'if-no-files-found': 'error', 'retention-days': 90 })) {
    fail('only the pinned artifact upload may follow the final writer; no post-writer mutation');
  }
}

/** Git blob SHA of a file (null when absent), comparable with the contents API `sha`. */
export function blobSha(bytes: Uint8Array | null): string | null {
  return bytes ? createHash('sha1').update(`blob ${bytes.length}\0`).update(bytes).digest('hex') : null;
}

export function assertWriterInputs(atSource: Record<string, string | null>, candidate: Record<string, string | null>) {
  // bunfig.toml can preload arbitrary code before the writer (thread r7s5f): forbid it outright rather than trace it.
  if (atSource['bunfig.toml'] !== null || candidate['bunfig.toml'] !== null) throw new Error('Golden writer inputs must not include a bunfig.toml (preload would run before the writer)');
  for (const file of WRITER_INPUTS) {
    if (!(file in atSource) || !(file in candidate) || atSource[file] !== candidate[file]) throw new Error(`Golden writer input at producer SHA differs from candidate: ${file}`);
  }
}
