/** Offline reporter contract: untrusted failure fields never reach files or logs.
 * Called by check_all in CI; no browser, services or credentials are required.
 */
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { resolve, join } from 'node:path';
import type { TestCase, TestResult } from '@playwright/test/reporter';
import SummaryReporter from '../../scripts/e2e/summary-reporter';

const temporary = mkdtempSync(join(tmpdir(), 'e2e-summary-'));
const outputFile = join(temporary, 'summary.json');
const canary = 'SYNTHETIC_PRIVATE_VALUE_374';
const source = resolve('tests/e2e/b2b.spec.ts');
const lines = readFileSync(source, 'utf8').split('\n');
const testLine = lines.findIndex(line => line.startsWith("test('real B2B")) + 1;
const assertionLine = lines.findIndex(line => line.includes("'Quote create status'")) + 1;
function check(ok: boolean) { if (!ok) throw new Error('Summary reporter contract failed'); }
const original = console.log;
const logs: string[] = [];
console.log = (...args: unknown[]) => { logs.push(args.join(' ')); };
try {
  const reporter = new SummaryReporter({ outputFile });
  const location = { file: source, line: assertionLine, column: 5 };
  const test = { location: { file: source, line: testLine, column: 1 },
    title: canary, titlePath: () => [canary],
    parent: { project: () => ({ name: 'desktop' }) } } as unknown as TestCase;
  const result = { status: 'failed', duration: 42, error: { message: canary, stack: canary, location },
    errors: [{ message: canary, location }], stdout: [canary], stderr: [canary],
    attachments: [{ name: canary, body: Buffer.from(canary), path: canary }],
    steps: [{ title: canary, category: 'expect', error: { message: canary }, location, steps: [] }] } as unknown as TestResult;
  reporter.onTestEnd(test, result);
  reporter.onEnd();
  const output = readFileSync(outputFile, 'utf8');
  check(!output.includes(canary) && !logs.join('').includes(canary));
  const [row] = JSON.parse(output);
  check(JSON.stringify(Object.keys(row).sort()) === JSON.stringify([
    'file', 'line', 'title_path', 'project', 'status', 'duration_ms', 'error_location', 'assertion_label'].sort()));
  check(row.status === 'failed' && row.duration_ms === 42 && row.project === 'desktop');
  check(row.error_location === `tests/e2e/b2b.spec.ts:${assertionLine}` && row.assertion_label === 'Quote create status');
  check(row.title_path.includes('real B2B inquiry quote approval prepayment order'));
  check(JSON.stringify(row) === logs[0]);
  // Unknown locations and non-expect errors must not become arbitrary output.
  reporter.onTestEnd(test, { ...result, error: { message: canary, location: { file: canary, line: 1, column: 1 } }, steps: [] } as TestResult);
  reporter.onEnd();
  check(!readFileSync(outputFile, 'utf8').includes(canary));
} finally {
  console.log = original;
  rmSync(temporary, { recursive: true, force: true });
}
console.log('PASS: summary reporter allowlist contract');
