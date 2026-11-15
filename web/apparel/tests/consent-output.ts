/** Built-output privacy and CSP gate; checks every emitted client file, no source mocks. */
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import { runInNewContext } from 'node:vm';
const root = process.env.APPAREL_DIST || 'web/apparel/dist';
const enabled = !!process.env.PUBLIC_GA4_ID;
const files = (dir: string): string[] => readdirSync(dir, { withFileTypes: true }).flatMap(e => e.isDirectory() ? files(join(dir, e.name)) : [join(dir, e.name)]);
const headers = readFileSync(join(root, '_headers'), 'utf8');
if (headers.includes('googletagmanager.com') !== enabled || headers.includes('google-analytics.com') !== enabled) throw Error('CSP analytics domains do not match build');
let pages = 0;
for (const file of files(root).filter(file => /\.(html|js|json|css|map)$/.test(file) && !file.includes('_worker.js/'))) {
  const text = readFileSync(file, 'utf8');
  if (!enabled && /gtag|google-analytics|googletagmanager/.test(text)) throw Error('Analytics leaked into disabled build');
  if (enabled && file.endsWith('.html')) {
    const script = [...text.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/g)].map(m => m[1]).find(s => /gtag\(["']consent["'],\s*["']default["']/.test(s));
    if (!script) throw Error('Missing consent default');
    if (!script.includes(JSON.stringify(process.env.PUBLIC_GA4_ID))) throw Error('Configured GA4 measurement ID missing');
    // Execute the emitted bootstrap with empty storage: inspect the actual initial consent command.
    const window = { dataLayer: [] as any[], addEventListener() {} };
    runInNewContext(script, { window }, { timeout: 1000 });
    const defaults = window.dataLayer.filter(row => row[0] === 'consent' && row[1] === 'default');
    if (defaults.length !== 1 || ['analytics_storage', 'ad_storage', 'ad_user_data', 'ad_personalization'].some(key => defaults[0][2][key] !== 'denied')) throw Error('Consent must default to denied');
    const hash = createHash('sha256').update(script).digest('base64');
    if (!headers.includes(`'sha256-${hash}'`)) throw Error('CSP does not authorize actual inline consent script');
    pages++;
  }
}
console.log(`PASS consent output: analytics ${enabled ? 'enabled' : 'absent'}, CSP verified (${pages} enabled pages)`);
