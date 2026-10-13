/** Exercises CI producer authority checks with real workflow source and faulted inputs; no network or tokens. */
import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { assertProducerWorkflow } from './workflow-provenance';
import { PLAYWRIGHT_IMAGE } from './baseline-provenance';

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
