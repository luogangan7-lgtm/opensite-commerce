/**
 * Responsibility: lock conservative renderer dependency-closure behavior before implementation.
 * Dependencies: Bun assertions and Node temporary-file APIs; loads scoped-inputs only inside tests.
 * Callers: CI frontend unit gate. Invariants: fixtures exist only during CI tests and are removed afterward.
 */
import { afterEach, expect, test } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';

type ScopedInputs = {
  graphInputFiles: (roots: string[], root: string) => string[];
  scopeInputs: (scope: 'gallery' | 'admin' | 'storefront-published' | 'storefront-commerce', root?: string) => string[];
};
const temporaryRoots: string[] = [];
afterEach(() => {
  for (const root of temporaryRoots.splice(0)) rmSync(root, { recursive: true, force: true });
});

function fixture(files: Record<string, string>): string {
  if (process.env.CI !== 'true') throw new Error('scoped-input fixtures require CI=true');
  const root = mkdtempSync(join(tmpdir(), 'c08c-scoped-inputs-'));
  temporaryRoots.push(root);
  for (const [file, source] of Object.entries(files)) {
    const path = join(root, file);
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, source);
  }
  return root;
}

async function loadInputs(): Promise<ScopedInputs> {
  // A variable URL defers missing-module failure to Bun's test rather than TypeScript resolution.
  const moduleURL = new URL('./scoped-inputs.ts', import.meta.url).href;
  return await import(moduleURL) as ScopedInputs;
}

test('follows transitive TS imports, reexports, index resolution and literal dynamic imports', async () => {
  const { graphInputFiles } = await loadInputs();
  const root = fixture({
    'src/entry.ts': "import './barrel'; void import('./lazy');",
    'src/barrel.ts': "export * from './widget'; export { value } from './leaf.js';",
    'src/widget/index.tsx': "import '../leaf'; export const widget = <div />;",
    'src/leaf.ts': 'export const value = 1;',
    'src/lazy.ts': "import './leaf';",
    'src/unrelated.ts': 'export const unrelated = 1;',
  });
  expect(graphInputFiles(['src/entry.ts'], root)).toEqual([
    'src/barrel.ts', 'src/entry.ts', 'src/lazy.ts', 'src/leaf.ts', 'src/widget/index.tsx',
  ]);
});

test('uses syntax nodes, ignores comments and import-like strings, and terminates cycles', async () => {
  const { graphInputFiles } = await loadInputs();
  const root = fixture({
    'src/a.ts': "import './b'; // import './missing';\nconst text = \"import './absent'\";",
    'src/b.ts': "export * from './a';",
  });
  expect(graphInputFiles(['src/a.ts'], root)).toEqual(['src/a.ts', 'src/b.ts']);
});

test('resolves workspace conditional and subpath exports and includes package metadata', async () => {
  const { graphInputFiles } = await loadInputs();
  const root = fixture({
    'package.json': JSON.stringify({ workspaces: ['packages/*'] }),
    'src/entry.ts': "import { card } from '@test/ui'; import '@test/ui/theme.css';",
    'packages/ui/package.json': JSON.stringify({ name: '@test/ui', exports: {
      '.': { types: './src/index.d.ts', import: './src/index.tsx' }, './theme.css': './theme.css',
    } }),
    'packages/ui/src/index.tsx': "export { card } from './card';",
    'packages/ui/src/index.d.ts': 'export declare const card: string;',
    'packages/ui/src/card.tsx': 'export const card = <span />;',
    'packages/ui/theme.css': ':root { --ink: black; }',
    'packages/unused/package.json': JSON.stringify({ name: '@test/unused', exports: './src/index.ts' }),
    'packages/unused/src/index.ts': 'export const unused = 1;',
  });
  const inputs = graphInputFiles(['src/entry.ts'], root);
  for (const file of ['package.json', 'packages/ui/package.json', 'packages/ui/src/index.tsx',
    'packages/ui/src/card.tsx', 'packages/ui/theme.css', 'src/entry.ts']) expect(inputs).toContain(file);
  expect(inputs).not.toContain('packages/unused/src/index.ts');
});

test('includes CSS imports and local url assets with query and fragment stripped', async () => {
  const { graphInputFiles } = await loadInputs();
  const root = fixture({
    'src/entry.ts': "import './app.css';",
    'src/app.css': '@import "./tokens.css"; @font-face { src: url("./font.woff2?v=1#face"); } .hero { background: url(./hero.svg); }',
    'src/tokens.css': '@import url("./palette.css"); .icon { background: url(data:image/png;base64,AAAA); }',
    'src/palette.css': ':root { --ink: black; }',
    'src/font.woff2': 'font bytes',
    'src/hero.svg': '<svg />',
  });
  expect(graphInputFiles(['src/entry.ts'], root)).toEqual([
    'src/app.css', 'src/entry.ts', 'src/font.woff2', 'src/hero.svg', 'src/palette.css', 'src/tokens.css',
  ]);
});

test('fails closed on nonliteral dynamic imports and unresolved local imports', async () => {
  const { graphInputFiles } = await loadInputs();
  const root = fixture({
    'src/dynamic.ts': 'const name = "./widget"; void import(name);',
    'src/missing.ts': "import './not-present';",
  });
  expect(() => graphInputFiles(['src/dynamic.ts'], root)).toThrow();
  expect(() => graphInputFiles(['src/missing.ts'], root)).toThrow();
});

test('fails closed when dependency escapes the declared repository root', async () => {
  const { graphInputFiles } = await loadInputs();
  const root = fixture({ 'src/entry.ts': "import '../../outside.ts';" });
  expect(() => graphInputFiles(['src/entry.ts'], root)).toThrow();
});
test('fails closed on unsupported package-import aliases and undeclared bare aliases', async () => {
  const { graphInputFiles } = await loadInputs();
  const root = fixture({
    'package.json': JSON.stringify({ imports: { '#theme': './src/theme.ts' } }),
    'src/entry.ts': "import '#theme';", 'src/other.ts': "import '~/view';",
    'src/theme.ts': 'export const color = 1;',
  });
  expect(() => graphInputFiles(['src/entry.ts'], root)).toThrow();
  expect(() => graphInputFiles(['src/other.ts'], root)).toThrow();
});

test('fails closed on Astro config Vite aliases that could hide a local leaf', async () => {
  const { graphInputFiles } = await loadInputs();
  const base = { 'package.json': JSON.stringify({ dependencies: { react: '1.0.0' } }), 'src/entry.ts': "import 'react';", 'src/shim.tsx': 'export default 1;' };
  const configs = [
    "export default { vite: { resolve: { alias: { react: '/src/shim.tsx' } } } };",
    "const resolve = {}; export default { vite: { resolve } };",
    "export default { vite: { ['resolve']: {} } };",
  ];
  for (const config of configs) expect(() => graphInputFiles(['src/entry.ts'], fixture({ ...base, 'astro.config.mjs': config }))).toThrow();
  expect(() => graphInputFiles(['src/entry.ts'], fixture({ ...base, 'astro.config.mjs': 'export default { vite: { define: { a: 1 } } };' }))).not.toThrow();
});

test('keeps distinct renderer roots independent while including their shared CSS', async () => {
  const { graphInputFiles } = await loadInputs();
  const root = fixture({
    'admin/entry.tsx': "import './orders'; import '../shared/styles.css';",
    'admin/orders.tsx': 'export const orders = <div />;',
    'storefront/entry.tsx': "import './checkout'; import '../shared/styles.css';",
    'storefront/checkout.tsx': 'export const checkout = <div />;',
    'shared/styles.css': '@import "./tokens.css";',
    'shared/tokens.css': ':root { --ink: black; }',
  });
  const admin = graphInputFiles(['admin/entry.tsx'], root);
  const storefront = graphInputFiles(['storefront/entry.tsx'], root);
  expect(admin).toEqual(['admin/entry.tsx', 'admin/orders.tsx', 'shared/styles.css', 'shared/tokens.css']);
  expect(storefront).toEqual(['shared/styles.css', 'shared/tokens.css', 'storefront/checkout.tsx', 'storefront/entry.tsx']);
});

test('follows Astro frontmatter, inline scripts and HTML script and stylesheet links', async () => {
  const { graphInputFiles } = await loadInputs();
  const root = fixture({
    'src/page.astro': "---\nimport Card from './card.tsx';\n---\n<Card /><script>import './client.mjs';</script><style>@import './page.css';</style>",
    'src/card.tsx': 'export default () => <div />;',
    'src/client.mjs': "export * from './leaf.js';",
    'src/leaf.js': 'export const value = 1;',
    'src/page.css': 'body { color: black; }',
    'src/index.html': '<script type="module" src="./client.mjs"></script><link rel="stylesheet" href="./page.css">',
  });
  expect(graphInputFiles(['src/page.astro', 'src/index.html'], root)).toEqual([
    'src/card.tsx', 'src/client.mjs', 'src/index.html', 'src/leaf.js', 'src/page.astro', 'src/page.css',
  ]);
});

test('C-08d: files imported by Vite/Astro configs are renderer inputs', async () => {
  const { graphInputFiles } = await loadInputs();
  const root = fixture({
    'package.json': JSON.stringify({ dependencies: { react: '1.0.0' } }),
    'src/entry.ts': "import 'react';",
    'astro.config.mjs': "import { hosts } from './src/hosts';\nexport default { vite: { define: { hosts } } };",
    'src/hosts.ts': "export { normalize as hosts } from './normalize';",
    'src/normalize.ts': 'export const normalize = 1;',
  });
  expect(graphInputFiles(['src/entry.ts'], root)).toEqual(expect.arrayContaining(['astro.config.mjs', 'src/hosts.ts', 'src/normalize.ts']));
});
test('C-08d: storefront scopes include the River migrator their fixture runs', async () => {
  const { scopeInputs } = await loadInputs();
  for (const scope of ['storefront-published', 'storefront-commerce'] as const) {
    const inputs = scopeInputs(scope);
    expect(inputs).toContain('web/storefront/src/lib/host-policy.ts');
    expect(inputs.some(path => path.startsWith('cmd/river-migrate/') && path.endsWith('.go'))).toBe(true);
  }
});
test('account history and verification renderer inputs belong to commerce provenance', async () => {
  const { scopeInputs } = await loadInputs();
  const inputs = scopeInputs('storefront-commerce');
  for (const path of ['pages/account/orders.astro', 'pages/customer/verify.astro',
    'components/OrderHistory.tsx', 'components/CustomerVerification.tsx']) {
    expect(inputs).toContain(`web/storefront/src/${path}`);
  }
});
