/** Exercises typed message and production boundaries; called by Bun/tsc; invalid dictionary keys remain compiler errors. */
import { expect, test } from 'bun:test';
import { t } from '../../packages/component-contracts/src';
import { readFileSync, readdirSync } from 'node:fs';

test('component copy is typed and complete', () => {
  expect(t('dialog.close')).toBe('Close dialog');
  // @ts-expect-error Missing dictionary keys must never become valid TypeScript.
  t('missing.component.key');
});

test('production cannot contain gallery code', () => {
  const directory = 'web/admin/dist/assets';
  for (const file of readdirSync(directory).filter(file => file.endsWith('.js'))) {
    expect(readFileSync(`${directory}/${file}`, 'utf8')).not.toContain('data-gallery-case');
  }
});

// Include cross-architecture CLI startup; rejection code and message remain mandatory.
test('local baseline generation is rejected', () => {
  const result = Bun.spawnSync(['bun', 'scripts/frontend/update-baselines.ts'], {
    env: { ...process.env, GITHUB_ACTIONS: 'false' }
  });
  expect(result.exitCode).toBe(1);
  expect(new TextDecoder().decode(result.stderr)).toContain('explicit pinned GitHub Actions');
}, 30_000);

test('direct local Playwright snapshot update is rejected before launch', () => {
  const result = Bun.spawnSync(['bun', 'x', '--no-install', 'playwright', 'test', '--update-snapshots', '--list'], {
    env: { ...process.env, GITHUB_ACTIONS: 'false' }
  });
  expect(result.exitCode).toBe(1);
  expect(new TextDecoder().decode(result.stderr)).toContain('Snapshot updates require the explicit pinned GitHub Actions');
}, 30_000);
