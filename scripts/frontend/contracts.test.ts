/** Exercises typed message and production boundaries; called by Bun/tsc; invalid dictionary keys remain compiler errors. */
import { expect, test } from 'bun:test';
import { messages as sharedMessages, t } from '../../packages/component-contracts/src';
import { readFileSync, readdirSync } from 'node:fs';
import { galleryMessages, t as galleryT } from '../../packages/component-contracts/src/gallery';

test('component copy is typed and complete', () => {
  expect(t('dialog.close')).toBe('Close dialog');
  // @ts-expect-error Missing dictionary keys must never become valid TypeScript.
  t('missing.component.key');
  // @ts-expect-error Gallery keys cannot leak into the shared dictionary.
  t('gallery.title');
  expect(galleryT('gallery.title')).toBe('Component library');
});

test('production build excludes gallery code, copy and CSS', () => {
  const directory = 'web/admin/dist/assets';
  const files = readdirSync(directory).filter(file => /\.(js|css)$/.test(file));
  expect(files.some(file => file.endsWith('.js'))).toBe(true);
  expect(files.some(file => file.endsWith('.css'))).toBe(true);
  for (const file of files) {
    const content = readFileSync(`${directory}/${file}`, 'utf8');
    expect(/data-gallery-case|data-testid|gallery[.-]/.test(content), file).toBe(false);
    for (const [key, message] of Object.entries(galleryMessages).filter(([, value]) => !(Object.values(sharedMessages) as readonly string[]).includes(value))) {
      // Match complete string literals, not React identifiers such as querySelector.
      expect(['\"', "'", '`'].some(quote => content.includes(`${quote}${message}${quote}`)), `${file}: ${key}`).toBe(false);
    }
  }
});

test('gallery build still includes its real copy and CSS', () => {
  const content = readdirSync('web/admin/dist-gallery/assets').filter(file => /\.(js|css)$/.test(file))
    .map(file => readFileSync(`web/admin/dist-gallery/assets/${file}`, 'utf8')).join('\n');
  expect(content.includes('Component library')).toBe(true);
  expect(content.includes('.gallery-layout')).toBe(true);
  expect(content.includes('data-gallery-case')).toBe(true);
});

test('local baseline generation is rejected', () => {
  const result = Bun.spawnSync(['bun', 'scripts/frontend/update-baselines.ts'], {
    env: { ...process.env, GITHUB_ACTIONS: 'false' }
  });
  expect(result.exitCode).toBe(1);
  expect(new TextDecoder().decode(result.stderr)).toContain('explicit pinned GitHub Actions');
});

test('direct local Playwright snapshot update is rejected before launch', () => {
  const result = Bun.spawnSync(['bun', 'x', '--no-install', 'playwright', 'test', '--update-snapshots', '--list'], {
    env: { ...process.env, GITHUB_ACTIONS: 'false' }
  });
  expect(result.exitCode).toBe(1);
  expect(new TextDecoder().decode(result.stderr)).toContain('Snapshot updates require the explicit pinned GitHub Actions');
});
