/** Locks Chromium/font rendering inputs; used by CI and container comparisons; CI alone can create golden images. */
import { defineConfig } from '@playwright/test';
import { PLAYWRIGHT_IMAGE } from './scripts/frontend/baseline-provenance';

if (process.argv.some(argument => argument.startsWith('--update-snapshots')) &&
    (process.env.GITHUB_ACTIONS !== 'true' || process.env.OPENSITE_BASELINE_UPDATE !== '1' ||
     process.env.PLAYWRIGHT_IMAGE !== PLAYWRIGHT_IMAGE || process.platform !== 'linux' || process.arch !== 'x64')) {
  throw new Error('Snapshot updates require the explicit pinned GitHub Actions container job');
}

export default defineConfig({
  testDir: './tests/frontend',
  timeout: 180_000,
  expect: { timeout: 10_000, toHaveScreenshot: { animations: 'disabled', threshold: 0, maxDiffPixels: 0 } },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  updateSnapshots: 'none',
  forbidOnly: true,
  reporter: [['list'], ['html', { open: 'never' }]],
  snapshotPathTemplate: '{testDir}/baselines/{projectName}/{arg}{ext}',
  use: {
    browserName: 'chromium',
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
    { command: 'bash scripts/start_storefront_api_e2e.sh', url: 'http://127.0.0.1:4175/healthz', reuseExistingServer: false, timeout: 180_000 },
    { command: 'bun run build:gallery && bun x --no-install vite preview --config web/admin/vite.config.ts web/admin --outDir dist-gallery --port 4173 --strictPort', url: 'http://127.0.0.1:4173', reuseExistingServer: false, timeout: 120_000 },
    { command: 'bun run build:admin && bun x --no-install vite preview --config web/admin/vite.config.ts web/admin --port 4174 --strictPort', url: 'http://127.0.0.1:4174', reuseExistingServer: false, timeout: 120_000 },
    { command: 'HOST=127.0.0.1 PORT=4321 OSC_READ_API_ORIGIN=http://127.0.0.1:4175 bun run start:storefront', url: 'http://127.0.0.1:4321', reuseExistingServer: false, timeout: 120_000 },
    { command: 'HOST=127.0.0.1 PORT=4322 OSC_READ_API_ORIGIN=http://127.0.0.1:1 bun run start:storefront', port: 4322, reuseExistingServer: false, timeout: 120_000 }
  ]
});
