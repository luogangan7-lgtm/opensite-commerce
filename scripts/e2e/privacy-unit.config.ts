/** Browserless API observer regression in the offline CI lane, using Node like MVP. */
import { defineConfig } from '@playwright/test';
export default defineConfig({
  testDir: '../../tests/e2e', testMatch: process.env.E2E_PRIVACY_BROWSER_SMOKE === 'true'
    ? ['privacy-harness.test.ts', 'privacy-fixture.test.ts'] : 'privacy-harness.test.ts',
  workers: 1, retries: 0, timeout: 30_000, reporter: 'line',
  outputDir: '../../test-results/e2e/privacy-unit',
});
