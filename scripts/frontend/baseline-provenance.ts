/** Hashes frontend inputs and PNGs; called by CI writer/comparison guard; provenance excludes artifacts, docs and receipts. */
import { createHash } from 'node:crypto';
import { readdirSync, readFileSync, statSync } from 'node:fs';
export const PLAYWRIGHT_IMAGE = 'mcr.microsoft.com/playwright:v1.64.0-noble@sha256:bc72a8df40831ded821e3cbc79c318c21b44d6e51dff9fd89ace7a8249c5b462';
export const BASELINES = 'tests/frontend/baselines';
export function filesAt(directory: string): string[] {
  return readdirSync(directory).sort().flatMap(name => {
    const path = `${directory}/${name}`;
    if (['node_modules', 'dist', 'dist-gallery', '.astro', 'baselines'].includes(name)) return [];
    return statSync(path).isDirectory() ? filesAt(path) : [path];
  });
}
export function sha256(path: string) { return createHash('sha256').update(readFileSync(path)).digest('hex'); }
export function sourceFingerprint() {
  const inputs = ['package.json', 'bun.lock', 'tsconfig.json', 'playwright.config.ts', 'scripts/check_frontend.sh',
    ...['packages', 'web/admin', 'web/storefront', 'scripts/frontend', 'tests/frontend'].flatMap(filesAt),
    'tests/fixtures/storefront_seed.sql', 'scripts/start_storefront_api_e2e.sh', 'scripts/seed_storefront.sh', 'scripts/start_storefront_ssr.sh',
    ...filesAt('internal/modules/publish'), 'internal/tenanttx/pgxadapter/public.go',
    'internal/platform/httpapi/storefront.go', 'cmd/api/storefront_pgx.go', 'api/openapi.yaml',
    ...filesAt('migrations').filter(path => path.endsWith('_storefront_read.sql'))
  ].filter(path => !path.endsWith('.md') && !path.endsWith('provenance.json')).sort();
  return createHash('sha256').update(inputs.map(path => `${path}\0${sha256(path)}\n`).join('')).digest('hex');
}
export function pngHashes(): Record<string, string> {
  const entries: [string, string][] = [];
  for (const project of ['desktop', 'mobile']) {
    for (const name of readdirSync(`${BASELINES}/${project}`).filter(name => name.endsWith('.png')).sort()) {
      const path = `${project}/${name}`;
      entries.push([path, sha256(`${BASELINES}/${path}`)]);
    }
  }
  return Object.fromEntries(entries);
}
