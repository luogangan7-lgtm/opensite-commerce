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
