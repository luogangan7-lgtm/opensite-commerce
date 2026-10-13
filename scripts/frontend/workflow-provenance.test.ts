/** Exercises CI producer authority checks with real workflow source and faulted inputs; no network or tokens. */
import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import * as implementation from './workflow-provenance';
import { PLAYWRIGHT_IMAGE } from './baseline-provenance';
import { graphInputFiles } from './scoped-inputs';

type API = {
  assertProducerWorkflow(path: string, source: string): void;
  assertWriterInputs(atSource: Record<string, string | null>, candidate: Record<string, string | null>): void;
  WRITER_INPUTS: string[];
};
const { assertProducerWorkflow, assertWriterInputs, WRITER_INPUTS } = implementation as unknown as API;

const path = '.github/workflows/checks.yml';
const source = readFileSync(path, 'utf8');
test('accepts the explicit pinned producer workflow', () => {
  expect(() => assertProducerWorkflow(path, source)).not.toThrow();
});
test('rejects an unrelated workflow with the same job name', () => {
  expect(() => assertProducerWorkflow('.github/workflows/other.yml', source)).toThrow();
});
test('rejects a producer running a different image', () => {
  expect(() => assertProducerWorkflow(path, source.replaceAll(PLAYWRIGHT_IMAGE, 'other:latest'))).toThrow();
});
test('rejects a missing explicit producer job', () => {
  expect(() => assertProducerWorkflow(path, source.replace('  frontend-baselines:', '  other-producer:'))).toThrow();
});
test('rejects missing CI generation command', () => {
  expect(() => assertProducerWorkflow(path, source.replace('run: bun run visual:update', 'run: echo no-generation'))).toThrow();
});
test('rejects a producer that rewrites images after the authorized writer', () => {
  const fault = source.replace('run: bun run visual:update', 'run: bun run visual:update\n      - run: echo forged > tests/frontend/baselines/desktop/gallery.png');
  expect(() => assertProducerWorkflow(path, fault)).toThrow(/after|final|upload/);
});
for (const modifier of ['if: false', 'env: {OPENSITE_BASELINE_UPDATE: "0"}', 'working-directory: other', 'shell: other']) {
  test(`rejects a producer that changes the authorized writer with ${modifier}`, () => {
    const fault = source.replace('run: bun run visual:update', `run: bun run visual:update\n        ${modifier}`);
    expect(() => assertProducerWorkflow(path, fault)).toThrow(/writer/);
  });
}
for (const level of ['workflow', 'job']) for (const modifier of ['shell: other', 'working-directory: other']) {
  test(`rejects inherited ${level} writer defaults with ${modifier}`, () => {
    const fault = level === 'workflow'
      ? source.replace('\njobs:', `\ndefaults:\n  run:\n    ${modifier}\njobs:`)
      : source.replace('  frontend-baselines:', `  frontend-baselines:\n    defaults:\n      run:\n        ${modifier}`);
    expect(() => assertProducerWorkflow(path, fault)).toThrow(/writer/);
  });
}

// C-08d (threads r23sS / r6FFa): nothing may run repository code or rewrite the writer before it executes.
const writerStep = '      - name: Generate golden images in pinned CI container\n';
const before = (step: string) => source.replace(writerStep, `      - ${step}\n${writerStep}`);
for (const step of [
  `run: echo '{"scripts":{"visual:update":"true"}}' > package.json`,
  'run: cp /tmp/forged.ts scripts/frontend/update-baselines.ts',
  'run: bash scripts/check_frontend.sh',
  'run: bun install --frozen-lockfile',
  'uses: actions/checkout@8e8c483db84b4bee98b60c0593521ed34d9990e8\n        with:\n          ref: attacker',
  'uses: attacker/action@8e8c483db84b4bee98b60c0593521ed34d9990e8',
  'run: go mod download\n        env: {GOFLAGS: -toolexec=./evil}',
]) {
  test(`rejects a pre-writer step outside the allowlist: ${step.split('\n')[0]}`, () => {
    expect(source.includes(writerStep)).toBe(true);
    expect(() => assertProducerWorkflow(path, before(step))).toThrow(/writer/);
  });
}
test('rejects producer env, workflow env, container or service changes that could preload code', () => {
  const update = "      OPENSITE_BASELINE_UPDATE: '1'\n";
  expect(source.includes(update)).toBe(true);
  expect(() => assertProducerWorkflow(path, source.replace(update, `${update}      BUN_OPTIONS: --preload ./evil.ts\n`))).toThrow(/writer/);
  expect(() => assertProducerWorkflow(path, source.replace('\njobs:', '\nenv:\n  NODE_OPTIONS: --require ./evil.js\njobs:'))).toThrow(/writer/);
  expect(() => assertProducerWorkflow(path, source.replace('  frontend-baselines:', '  frontend-baselines:\n    strategy:\n      matrix: {a: [1]}'))).toThrow(/writer/);
  const services = source.slice(source.indexOf('  frontend-baselines:'));
  const faulted = services.replace('      postgres:\n', '      postgres:\n        volumes: ["/__w:/w"]\n');
  expect(faulted).not.toBe(services);
  expect(() => assertProducerWorkflow(path, source.replace(services, faulted))).toThrow(/writer/);
});
test('the writer inputs list covers every local file the writer loads', () => {
  const closure = graphInputFiles(['scripts/frontend/update-baselines.ts'], process.cwd())
    .filter(file => !file.endsWith('package.json') && !/(^|\/)tsconfig[^/]*\.json$/.test(file));
  expect(closure.filter(file => !WRITER_INPUTS.includes(file))).toEqual([]);
  for (const file of ['package.json', 'bun.lock', 'bunfig.toml', 'tsconfig.json']) expect(WRITER_INPUTS).toContain(file);
});
test('rejects a golden whose writer inputs at the producer SHA differ from the candidate tree', () => {
  const same = Object.fromEntries(WRITER_INPUTS.map(file => [file, file === 'bunfig.toml' ? null : 'a'.repeat(40)]));
  expect(() => assertWriterInputs(same, same)).not.toThrow();
  expect(() => assertWriterInputs({ ...same, 'scripts/frontend/update-baselines.ts': 'b'.repeat(40) }, same)).toThrow(/writer/);
  expect(() => assertWriterInputs({ ...same, 'bunfig.toml': 'c'.repeat(40) }, same)).toThrow(/writer/);
  // Thread r7s5f: identical bunfig with a preload on both sides is still refused (the preload file is not bound).
  const preload = { ...same, 'bunfig.toml': 'd'.repeat(40) };
  expect(() => assertWriterInputs(preload, preload)).toThrow(/bunfig/);
  expect(() => assertWriterInputs({}, same)).toThrow(/writer/);
});
// Thread r7s5r: the PostgreSQL service is an exact allowlist, not "no mount flags".
for (const [label, from, to] of [
  ['privileged', '--health-retries 20', '--health-retries 20 --privileged'],
  ['host PID', '--health-retries 20', '--health-retries 20 --pid=host'],
  ['custom healthcheck', 'pg_isready -U postgres -d opensite', 'sh -c \'cp /x /proc/1/root/w\''],
  ['entrypoint', '--health-retries 20', '--health-retries 20 --entrypoint /bin/sh'],
  ['extra env', "POSTGRES_PASSWORD: ci_test_only\n", "POSTGRES_PASSWORD: ci_test_only\n          LD_PRELOAD: /x.so\n"],
] as const) {
  test(`rejects a producer service with ${label}`, () => {
    const producer = source.slice(source.indexOf('  frontend-baselines:'));
    const faulted = producer.replace(from, to);
    expect(faulted).not.toBe(producer);
    expect(() => assertProducerWorkflow(path, source.replace(producer, faulted))).toThrow(/writer/);
  });
}
// Independent review P2-1 on #103: Bun loads .env* into the writer, like bunfig.toml; only .env.example may exist.
test('rejects .env files at the producer SHA or in the candidate', () => {
  const { assertNoBunEnvFiles } = implementation as unknown as { assertNoBunEnvFiles(names: string[]): void };
  expect(() => assertNoBunEnvFiles(['.env.example', 'package.json', 'bun.lock'])).not.toThrow();
  for (const name of ['.env', '.env.local', '.env.development', '.env.production.local', '.env.test']) {
    expect(() => assertNoBunEnvFiles(['package.json', name])).toThrow(/writer/);
  }
});
