/** CI-only static preview runner; Playwright owns lifecycle and timeout. */
import { defineConfig } from '@playwright/test';
export default defineConfig({ testDir: './tests', testMatch: '*.spec.ts', timeout: 30000, use: { baseURL: 'http://127.0.0.1:4322', viewport: { width: 390, height: 844 } }, webServer: { command: 'bun run preview', url: 'http://127.0.0.1:4322', timeout: 30000 }, reporter: 'list' });
