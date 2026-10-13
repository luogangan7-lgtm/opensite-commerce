/** Fails a fully passing CI comparison when a committed golden PNG was never compared (orphan of a removed or
 * renamed screenshot). Depends on Playwright's reporter API and baseline-provenance's PNG inventory; loaded by
 * playwright.config.ts and active only with OPENSITE_ORPHAN_CHECK=1 (the CI frontend job's full run), because a
 * filtered or failing run legitimately leaves goldens unreached. An unparseable step title fails closed. */
import type { FullResult, Reporter, TestCase, TestResult, TestStep } from '@playwright/test/reporter';
import { pngHashes } from './baseline-provenance';

export default class GoldenUsageReporter implements Reporter {
  private used = new Set<string>();

  onStepEnd(test: TestCase, _result: TestResult, step: TestStep) {
    const name = step.category === 'expect' ? /toHaveScreenshot\(([^)]+)\)/.exec(step.title)?.[1] : undefined;
    if (name) this.used.add(`${test.parent.project()?.name}/${name}`);
  }

  onEnd(result: FullResult) {
    if (process.env.OPENSITE_ORPHAN_CHECK !== '1' || result.status !== 'passed') return;
    const orphans = Object.keys(pngHashes()).filter(snapshot => !this.used.has(snapshot));
    if (orphans.length === 0) return;
    console.error(`FAIL: ${orphans.length} committed golden(s) never compared; delete them with their records:\n  ${orphans.join('\n  ')}`);
    return { status: 'failed' as const };
  }
}
