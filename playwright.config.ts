/** Locks Chromium/font rendering inputs; used by CI and container comparisons; CI alone can create golden images. */
import { defineConfig } from '@playwright/test';
import { PLAYWRIGHT_IMAGE, RENDERING } from './scripts/frontend/baseline-provenance';

if (process.argv.some(argument => argument.startsWith('--update-snapshots')) &&
    (process.env.GITHUB_ACTIONS !== 'true' || process.env.OPENSITE_BASELINE_UPDATE !== '1' ||
     process.env.PLAYWRIGHT_IMAGE !== PLAYWRIGHT_IMAGE || process.platform !== 'linux' || process.arch !== 'x64')) {
  throw new Error('Snapshot updates require the explicit pinned GitHub Actions container job');
}

export default defineConfig({
  testDir: './tests/frontend',
  // The renderer-lifecycle regression suite next to the spec is a Bun test, not a browser test.
  testMatch: '**/*.spec.ts',
  timeout: 180_000,
  expect: { timeout: 10_000, toHaveScreenshot: { animations: 'disabled', caret: 'hide', threshold: 0, maxDiffPixels: 0 } },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  updateSnapshots: 'none',
  forbidOnly: true,
  reporter: [['list'], ['html', { open: 'never' }], ['./scripts/frontend/golden-usage-reporter.ts']],
  snapshotPathTemplate: '{testDir}/baselines/{projectName}/{arg}{ext}',
  use: {
    browserName: 'chromium',
    launchOptions: { args: RENDERING.chromiumArgs },
    baseURL: 'http://127.0.0.1:4173',
    locale: 'en-US', timezoneId: 'UTC', colorScheme: 'light',
    reducedMotion: 'reduce', deviceScaleFactor: 1,
    trace: 'retain-on-failure', screenshot: 'only-on-failure'
  },
  projects: [
    { name: 'desktop', use: { viewport: { width: 1280, height: 800 } } },
    { name: 'mobile', use: { viewport: { width: 390, height: 844 } } }
  ],
  webServer: [
    { command: 'OPENSITE_ENVIRONMENT=test OSC_COMMERCE_TEST_HTTP=1 bun run build:storefront && bash scripts/start_storefront_api_e2e.sh', url: 'http://127.0.0.1:4175/healthz', reuseExistingServer: false, timeout: 180_000 },
    { command: 'bun run build:gallery && bun x --no-install vite preview --config web/admin/vite.config.ts web/admin --outDir dist-gallery --port 4173 --strictPort', url: 'http://127.0.0.1:4173', reuseExistingServer: false, timeout: 120_000 },
    { command: 'bun run build:admin && bun x --no-install vite preview --config web/admin/vite.config.ts web/admin --port 4174 --strictPort', url: 'http://127.0.0.1:4174', reuseExistingServer: false, timeout: 120_000 },
    { command: 'HOST=127.0.0.1 PORT=4321 OSC_READ_API_ORIGIN=http://127.0.0.1:4175 bun run start:storefront', url: 'http://127.0.0.1:4321', reuseExistingServer: false, timeout: 120_000 },
    { command: 'bun tests/frontend/commerce-fixture.ts', url: 'http://127.0.0.1:4176/healthz', reuseExistingServer: false, timeout: 30_000 },
    { command: 'HOST=127.0.0.1 PORT=4323 OSC_READ_API_ORIGIN=http://127.0.0.1:4175 OSC_COMMERCE_FIXTURE_ORIGIN=http://127.0.0.1:4176 OSC_COMMERCE_FIXTURE_SITE=http://127.0.0.1:4323 OSC_COMMERCE_FIXTURE_MODE=contract-test bun run start:storefront', url: 'http://127.0.0.1:4323', reuseExistingServer: false, timeout: 120_000 },
    // Existing test-only HTTP capability enables the actual backend-mode account document.
    // Browser routes supply safe state DTOs; no backend identity or payment acceptance is implied.
    { command: `HOST=127.0.0.1 PORT=4324 OPENSITE_ENVIRONMENT=test OSC_COMMERCE_TEST_HTTP=1 OSC_READ_API_ORIGIN=http://127.0.0.1:4175 OSC_COMMERCE_MODE=backend OSC_COMMERCE_SITE=http://127.0.0.1:4324 OSC_COMMERCE_API_ORIGIN=http://127.0.0.1:4175 OSC_COMMERCE_COOKIE_KEY="$(node -e 'process.stdout.write(require("node:crypto").randomBytes(32).toString("base64url"))')" bun run start:storefront`, url: 'http://127.0.0.1:4324/account', reuseExistingServer: false, timeout: 120_000 },
    { command: 'HOST=127.0.0.1 PORT=4322 OSC_READ_API_ORIGIN=http://127.0.0.1:1 bun run start:storefront', port: 4322, reuseExistingServer: false, timeout: 120_000 },
    { command: 'python3 scripts/frontend/start-real-backend.py', url: 'http://127.0.0.1:4325/products/canvas-tote', reuseExistingServer: false, timeout: 300_000, gracefulShutdown: { signal: 'SIGTERM', timeout: 15_000 } }
  ]
});
