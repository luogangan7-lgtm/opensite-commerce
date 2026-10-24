/** Responsibility: run the separate real-stack browser gate with diagnostic artifacts.
 * Dependencies: pinned Playwright and guarded CI fixture; caller: mvp-e2e CI job.
 * Invariants: no local server spawning, no retries masking defects, no gallery or snapshots.
 */
import { defineConfig } from '@playwright/test';
import { fixture } from './tests/e2e/fixture';

const phase = process.env.E2E_BROWSER_PHASE;
if (!['base', 'privacy'].includes(phase ?? '')) throw new Error('Explicit CI browser phase required');
const privacy = phase === 'privacy';
const privacyJourney = /real registered customer export and erasure journey/;
export default defineConfig({
  ...(privacy ? { grep: privacyJourney } : { grepInvert: privacyJourney }),
  testDir: './tests/e2e',
  testMatch: '**/*.spec.ts',
  timeout: 60_000,
  globalTimeout: 42 * 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: true,
  updateSnapshots: 'none',
  outputDir: `test-results/e2e/${phase}`,
  reporter: [
    ['list'],
    ['json', { outputFile: `test-results/e2e/${phase}/results.json` }],
    ['html', { open: 'never', outputFolder: `playwright-report/e2e/${phase}` }],
  ],
  use: {
    browserName: 'chromium', baseURL: fixture.storefront,
    locale: 'en-US', timezoneId: 'UTC', colorScheme: 'light',
    reducedMotion: 'reduce', deviceScaleFactor: 1,
    trace: 'off', video: 'off', screenshot: 'off',
  },
  projects: [
    { name: 'desktop', use: { viewport: { width: 1280, height: 800 } } },
    { name: 'mobile', use: { viewport: { width: 390, height: 844 } } },
  ],
});
