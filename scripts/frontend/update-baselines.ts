/** Produces golden PNGs only in the explicit GitHub Actions container job; developers download its artifact. */
import { mkdirSync, writeFileSync } from 'node:fs';
import { BASELINES, PLAYWRIGHT_IMAGE, pngHashes, sourceFingerprint } from './baseline-provenance';
if (process.env.GITHUB_ACTIONS !== 'true' || process.env.OPENSITE_BASELINE_UPDATE !== '1' ||
    process.env.PLAYWRIGHT_IMAGE !== PLAYWRIGHT_IMAGE || process.platform !== 'linux' || process.arch !== 'x64' ||
    !process.env.GITHUB_RUN_ID || !process.env.GITHUB_SHA || !process.env.GITHUB_REPOSITORY) {
  console.error('FAIL: baseline updates require the explicit pinned GitHub Actions container job');
  process.exit(1);
}
const result = Bun.spawnSync(['bun', 'x', '--no-install', 'playwright', 'test', '--update-snapshots'], { stdout: 'inherit', stderr: 'inherit' });
if (result.exitCode !== 0) process.exit(result.exitCode);
mkdirSync(BASELINES, { recursive: true });
writeFileSync(`${BASELINES}/provenance.json`, JSON.stringify({
  sourceSha: process.env.GITHUB_SHA,
  runUrl: `https://github.com/${process.env.GITHUB_REPOSITORY}/actions/runs/${process.env.GITHUB_RUN_ID}`,
  image: PLAYWRIGHT_IMAGE, playwright: '1.64.0', font: '@fontsource/inter@5.3.0',
  rendering: { locale: 'en-US', timezone: 'UTC', deviceScaleFactor: 1, colorScheme: 'light', reducedMotion: 'reduce', desktop: [1280, 800], mobile: [390, 844] },
  sourceFingerprint: sourceFingerprint(), pngHashes: pngHashes()
}, null, 2) + '\n');
