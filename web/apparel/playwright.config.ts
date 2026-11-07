/** CI-only static preview runner; Playwright owns lifecycle and timeout. */
import { defineConfig } from '@playwright/test';
const preview = process.env.APPAREL_PREVIEW_URL;
if (preview && !/^https:\/\/[a-z0-9-]+\.sourcyo-apparel\.pages\.dev$/.test(preview)) throw Error('Only Sourcyo Pages preview origins are allowed');
export default defineConfig({ testDir: './tests', testMatch: '*.spec.ts', timeout: 30000, use: { baseURL: preview || 'http://127.0.0.1:4322', viewport: { width: 390, height: 844 } }, webServer: preview ? undefined : { command: 'bun run preview', url: 'http://127.0.0.1:4322', timeout: 30000 }, reporter: 'list' });
