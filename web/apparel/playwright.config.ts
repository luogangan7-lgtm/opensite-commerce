/** Static preview runner; Playwright owns lifecycle; consent tests enforce generated CSP themselves. */
import { defineConfig } from '@playwright/test';
const port = Number(process.env.APPAREL_TEST_PORT || 4322);
if (!Number.isInteger(port) || port < 1024 || port > 65535) throw Error('Invalid test port');
const preview = process.env.APPAREL_PREVIEW_URL;
if (preview && !/^https:\/\/[a-z0-9-]+\.sourcyo-apparel\.pages\.dev$/.test(preview)) throw Error('Only Sourcyo Pages preview origins are allowed');
export default defineConfig({ testDir: './tests', workers: 1, testMatch: '*.spec.ts', timeout: 30000, use: { baseURL: preview || `http://127.0.0.1:${port}`, viewport: { width: 390, height: 844 } }, webServer: preview ? undefined : [{ command: `python3 -m http.server ${port} --bind 127.0.0.1 --directory dist`, url: `http://127.0.0.1:${port}`, timeout: 30000 }, { command: `bunx wrangler@4.156.0 pages dev dist --port ${port + 1} --ip 127.0.0.1`, url: `http://127.0.0.1:${port + 1}`, timeout: 60000 }], reporter: 'list' });
