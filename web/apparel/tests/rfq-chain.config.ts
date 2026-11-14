/** RFQ fixture runner: static local output only, foreground server owned by Playwright. */
import { defineConfig } from '@playwright/test';
import { fileURLToPath } from 'node:url';
const dist = fileURLToPath(new URL('../dist', import.meta.url));
export default defineConfig({ testDir: '.', testMatch: ['rfq-chain.spec.ts', 'foundation.spec.ts'], workers: 1, timeout: 30000, use: { baseURL: 'http://127.0.0.1:4328', viewport: { width: 390, height: 844 } }, webServer: { command: `python3 -m http.server 4328 --bind 127.0.0.1 --directory "${dist}"`, url: 'http://127.0.0.1:4328', timeout: 10000 }, reporter: 'list' });
