/** Browserless API observer regression in the offline CI lane, using Node like MVP. */
import { defineConfig } from '@playwright/test';
export default defineConfig({
  testDir: '../../tests/e2e', testMatch: 'privacy-harness.test.ts',
  workers: 1, retries: 0, timeout: 30_000, reporter: 'line',
  outputDir: '../../work/FIX-374/privacy-unit',
});
