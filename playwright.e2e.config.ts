/** Responsibility: run the separate real-stack browser gate with diagnostic artifacts.
 * Dependencies: pinned Playwright and guarded CI fixture; caller: CI e2e:test.
 * Invariants: no local server spawning, no retries masking defects, no gallery or snapshots.
 */
import { defineConfig } from '@playwright/test';
import { fixture } from './tests/e2e/fixture';

export default defineConfig({
  testDir: './tests/e2e',
  testMatch: '**/*.spec.ts',
  timeout: 60_000,
  globalTimeout: 10 * 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: true,
  updateSnapshots: 'none',
  outputDir: 'test-results/e2e',
  reporter: [
    ['list'],
    ['json', { outputFile: 'test-results/e2e/results.json' }],
    ['html', { open: 'never', outputFolder: 'playwright-report/e2e' }],
  ],
  use: {
    browserName: 'chromium', baseURL: fixture.storefront,
    locale: 'en-US', timezoneId: 'UTC', colorScheme: 'light',
    reducedMotion: 'reduce', deviceScaleFactor: 1,
    trace: 'retain-on-failure', screenshot: 'only-on-failure',
  },
  projects: [
    { name: 'desktop', use: { viewport: { width: 1280, height: 800 } } },
    { name: 'mobile', use: { viewport: { width: 390, height: 844 } } },
  ],
});
