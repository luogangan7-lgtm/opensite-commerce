/**
 * Responsibility: derive conservative renderer input closures from entrypoints, never hand-list transitive inputs.
 * Dependencies: TypeScript AST and Node filesystem/path APIs; no baseline-provenance import or database writes.
 * Callers: baseline provenance writer and consumer, CI unit tests.
 * Invariants: sorted repository-relative files, workspace metadata included, unknown local/dynamic dependencies fail closed.
 */
import ts from 'typescript-parser';
import { isBuiltin } from 'node:module';
import { existsSync, readFileSync, readdirSync, realpathSync, statSync } from 'node:fs';
import { dirname, extname, isAbsolute, join, relative, resolve, sep } from 'node:path';

export type RendererScope = 'gallery' | 'admin' | 'storefront-published' | 'storefront-commerce' | 'real-backend';
type PackageMetadata = { name?: string; workspaces?: string[] | { packages?: string[] }; exports?: unknown; main?: string; module?: string; types?: string;
  dependencies?: Record<string, string>; devDependencies?: Record<string, string>; peerDependencies?: Record<string, string>; optionalDependencies?: Record<string, string> };
const extensions = ['.ts', '.tsx', '.js', '.jsx', '.mjs', '.cjs', '.astro', '.css', '.json', '.d.ts'];

function metadata(path: string): PackageMetadata {
  return JSON.parse(readFileSync(path, 'utf8')) as PackageMetadata;
}

function filesIn(path: string): string[] {
  if (!existsSync(path)) return [];
  if (!statSync(path).isDirectory()) return [path];
  return readdirSync(path).sort().flatMap(name =>
    ['node_modules', 'dist', 'dist-gallery', '.astro', '.git'].includes(name) ? [] : filesIn(join(path, name)));
}

/** Public graph contract: roots are source/spec entries, not a whole-tree fallback. */
export function graphInputFiles(roots: string[], root: string): string[] {
  const repository = realpathSync(root);
  const found = new Set<string>();
  const visited = new Set<string>();
  const packages = new Map<string, { directory: string; path: string; data: PackageMetadata }>();
  const externals = new Map<string, Set<string>>();
  const repositoryPackage = join(repository, 'package.json');
  const contained = (path: string): string => {
    const local = relative(repository, path);
    if (local === '..' || local.startsWith(`..${sep}`) || isAbsolute(local)) throw new Error(`Dependency escapes repository: ${path}`);
    return path;
  };
  const record = (path: string): string => {
    contained(path);
    if (!existsSync(path) || !statSync(path).isFile()) throw new Error(`Missing input: ${path}`);
    contained(realpathSync(path));
    return relative(repository, path).split(sep).join('/');
  };
  const workspaceDirectories = (pattern: string): string[] => {
    let directories = [repository];
    for (const segment of pattern.split('/')) {
      if (segment !== '*' && segment.includes('*')) throw new Error(`Unsupported workspace pattern: ${pattern}`);
      directories = directories.flatMap(directory => segment === '*'
        ? readdirSync(directory).filter(name => statSync(join(directory, name)).isDirectory()).map(name => join(directory, name))
        : existsSync(join(directory, segment)) ? [contained(join(directory, segment))] : []);
    }
    return directories;
  };
  const declareExternals = (path: string, data: PackageMetadata): void => {
    externals.set(path, new Set(Object.keys({ ...data.dependencies, ...data.devDependencies, ...data.peerDependencies, ...data.optionalDependencies })));
  };
  if (existsSync(repositoryPackage)) {
    record(repositoryPackage);
    const data = metadata(repositoryPackage);
    declareExternals(repositoryPackage, data);
    const workspace = data.workspaces;
    const patterns = Array.isArray(workspace) ? workspace : workspace?.packages ?? [];
    for (const directory of patterns.flatMap(workspaceDirectories)) {
      const path = join(directory, 'package.json');
      if (!existsSync(path)) continue;
      record(path);
      const data = metadata(path);
      declareExternals(path, data);
      if (data.name) {
        if (packages.has(data.name)) throw new Error(`Duplicate workspace package: ${data.name}`);
        packages.set(data.name, { directory, path, data });
      }
    }
  }
  const localFile = (path: string): string => {
    contained(path);
    const suffix = extname(path);
    const replacement = /\.(?:mjs|cjs|js|jsx)$/.test(path) ? [path.replace(/\.[^.]+$/, '.ts'), path.replace(/\.[^.]+$/, '.tsx')] : [];
    const candidates = [path, ...replacement, ...(!suffix ? extensions.map(extension => path + extension) : []),
      ...(!suffix || existsSync(path) && statSync(path).isDirectory() ? extensions.map(extension => join(path, `index${extension}`)) : [])];
    const result = candidates.find(candidate => existsSync(candidate) && statSync(candidate).isFile());
    if (!result) throw new Error(`Unresolved local dependency: ${relative(repository, path)}`);
    record(result);
    return result;
  };
  const packageRoot = (path: string): string => {
    let directory = dirname(path);
    while (directory !== repository) {
      if (existsSync(join(directory, 'package.json'))) return directory;
      directory = dirname(directory);
    }
    return repository;
  };
  const checkedConfigs = new Set<string>();
  // Vite/Astro configs are traced like sources once the graph is defined, so files they import count as inputs.
  const configEntries: string[] = [];
  const rejectAliases = (directory: string): void => {
    if (checkedConfigs.has(directory)) return;
    checkedConfigs.add(directory);
    const config = (path: string): void => {
      if (checkedConfigs.has(path)) return;
      checkedConfigs.add(path);
      found.add(record(path));
      const parsed = ts.parseConfigFileTextToJson(path, readFileSync(path, 'utf8'));
      if (parsed.error) throw new Error(`Invalid dependency config: ${path}`);
      const data = parsed.config as { compilerOptions?: { paths?: Record<string, unknown>; baseUrl?: unknown }; extends?: string | string[] };
      if (data.compilerOptions?.baseUrl !== undefined || Object.keys(data.compilerOptions?.paths ?? {}).length) throw new Error(`Unsupported local alias config: ${path}`);
      for (const parent of [data.extends].flat().filter((value): value is string => typeof value === 'string')) {
        if (parent.startsWith('.') || isAbsolute(parent)) config(localFile(resolve(dirname(path), parent.endsWith('.json') ? parent : `${parent}.json`)));
      }
    };
    for (const name of readdirSync(directory).filter(name => /^(?:tsconfig|jsconfig)(?:\.[^.]+)?\.json$/.test(name))) config(join(directory, name));
    for (const name of readdirSync(directory).filter(name => /^(?:vite|astro)\.config\.(?:[cm]?[jt]s)$/.test(name))) {
      const path = join(directory, name);
      found.add(record(path));
      configEntries.push(path);
      const source = ts.createSourceFile(path, readFileSync(path, 'utf8'), ts.ScriptTarget.Latest, true);
      const scan = (node: ts.Node): void => {
        if (ts.isShorthandPropertyAssignment(node) && ['resolve', 'alias'].includes(node.name.text)) throw new Error(`Unsupported Vite resolution config: ${path}`);
        if (ts.isPropertyAssignment(node)) {
          const name = ts.isComputedPropertyName(node.name) ? node.name.expression.getText(source) : node.name.getText(source);
          if (['resolve', 'alias'].includes(name.replace(/^['"]|['"]$/g, ''))) throw new Error(`Unsupported Vite resolution config: ${path}`);
        }
        if (ts.isPropertyAccessExpression(node) && node.name.text === 'alias') throw new Error(`Unsupported Vite alias config: ${path}`);
        if (ts.isElementAccessExpression(node) && ts.isStringLiteral(node.argumentExpression) && node.argumentExpression.text === 'alias') throw new Error(`Unsupported Vite alias config: ${path}`);
        ts.forEachChild(node, scan);
      };
      scan(source);
    }
  };
  rejectAliases(repository);
  const exportTargets = (value: unknown): string[] => {
    if (typeof value === 'string') return [value];
    if (Array.isArray(value)) return value.flatMap(exportTargets);
    if (value && typeof value === 'object') return Object.values(value).flatMap(exportTargets);
    return [];
  };
  const dependency = (specifier: string, from: string, asset = false): void => {
    if (!asset && specifier.startsWith('#')) throw new Error(`Unsupported package-import alias: ${specifier}`);
    rejectAliases(packageRoot(from));
    const clean = specifier.replace(/[?#].*$/, '');
    if (!clean || /^(?:data:|blob:)/.test(clean) || isBuiltin(clean) || clean === 'bun' || clean === 'bun:test') return;
    if (/^(?:https?:)?\/\//.test(clean)) throw new Error(`Untracked remote dependency: ${specifier}`);
    if (clean.startsWith('.')) { visit(localFile(resolve(dirname(from), clean))); return; }
    if (clean.startsWith('/')) {
      const directory = packageRoot(from);
      const source = join(directory, clean.slice(1));
      const publicAsset = join(directory, 'public', clean.slice(1));
      visit(localFile(existsSync(source) ? source : publicAsset));
      return;
    }
    if (asset) { visit(localFile(resolve(dirname(from), clean))); return; }
    const name = clean.startsWith('@') ? clean.split('/').slice(0, 2).join('/') : clean.split('/')[0]!;
    const workspace = packages.get(name);
    // External libraries are bound by the common lockfile; local workspace packages are traced in source.
    if (!workspace) {
      if (clean.startsWith('@opensite/')) throw new Error(`Unresolved workspace dependency: ${clean}`);
      const provider = [join(packageRoot(from), 'package.json'), repositoryPackage].find(path => externals.get(path)?.has(name));
      if (!provider || clean.startsWith('node:')) throw new Error(`Undeclared bare dependency: ${clean}`);
      found.add(record(provider));
      return;
    }
    found.add(record(repositoryPackage));
    found.add(record(workspace.path));
    const subpath = clean === name ? '.' : `.${clean.slice(name.length)}`;
    let targets: string[];
    const exports = workspace.data.exports;
    if (exports && typeof exports === 'object' && !Array.isArray(exports) && Object.keys(exports).some(key => key.startsWith('.'))) {
      const entries = exports as Record<string, unknown>;
      if (subpath in entries) targets = exportTargets(entries[subpath]);
      else {
        const key = Object.keys(entries).filter(key => key.includes('*')).sort((a, b) => b.length - a.length).find(key => {
          const [prefix, suffix] = key.split('*');
          return subpath.startsWith(prefix!) && subpath.endsWith(suffix!);
        });
        if (!key) throw new Error(`Unresolved workspace export: ${clean}`);
        const [prefix, suffix] = key.split('*');
        const substitution = subpath.slice(prefix!.length, suffix!.length ? -suffix!.length : undefined);
        targets = exportTargets(entries[key]).map(target => target.replaceAll('*', substitution));
      }
    } else if (exports !== undefined) targets = subpath === '.' ? exportTargets(exports) : [];
    else targets = subpath === '.' ? [workspace.data.module, workspace.data.main, workspace.data.types].filter((value): value is string => !!value) : [subpath];
    if (!targets.length) throw new Error(`Unresolved workspace export: ${clean}`);
    for (const target of targets) {
      if (!target.startsWith('./')) throw new Error(`Invalid workspace export target: ${target}`);
      visit(localFile(resolve(workspace.directory, target)));
    }
  };
  const script = (text: string, path: string): void => {
    const kind = /\.[jt]sx$/.test(path) ? ts.ScriptKind.TSX : ts.ScriptKind.TS;
    const source = ts.createSourceFile(path, text, ts.ScriptTarget.Latest, true, kind);
    const literal = (node: ts.Node | undefined): string => {
      if (node && (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node))) return node.text;
      throw new Error(`Nonliteral dependency in ${relative(repository, path)}`);
    };
    const walk = (node: ts.Node): void => {
      if ((ts.isImportDeclaration(node) || ts.isExportDeclaration(node)) && node.moduleSpecifier) dependency(literal(node.moduleSpecifier), path);
      else if (ts.isImportEqualsDeclaration(node) && ts.isExternalModuleReference(node.moduleReference)) dependency(literal(node.moduleReference.expression), path);
      else if (ts.isImportTypeNode(node) && ts.isLiteralTypeNode(node.argument)) dependency(literal(node.argument.literal), path);
      else if (ts.isCallExpression(node) && (node.expression.kind === ts.SyntaxKind.ImportKeyword || ts.isIdentifier(node.expression) && node.expression.text === 'require')) dependency(literal(node.arguments[0]), path);
      else if (ts.isCallExpression(node) && /^import\.meta\.glob(?:Eager)?$/.test(node.expression.getText(source))) throw new Error(`Untracked glob dependency in ${relative(repository, path)}`);
      else if (ts.isNewExpression(node) && ts.isIdentifier(node.expression) && node.expression.text === 'URL' && node.arguments?.[1]?.getText(source) === 'import.meta.url') dependency(literal(node.arguments[0]), path, true);
      ts.forEachChild(node, walk);
    };
    walk(source);
  };
  const css = (text: string, path: string): void => {
    const clean = text.replace(/\/\*[\s\S]*?\*\//g, '');
    for (const match of clean.matchAll(/@import\s+(['"])(.*?)\1/gi)) dependency(match[2]!, path, true);
    for (const match of clean.matchAll(/url\(\s*(?:"([^"]*)"|'([^']*)'|([^)]*))\s*\)/gi)) dependency((match[1] ?? match[2] ?? match[3]!).trim(), path, true);
  };
  const markup = (text: string, path: string): void => {
    const clean = text.replace(/<!--[\s\S]*?-->/g, '');
    const attribute = (tag: string, name: string): string | undefined => new RegExp(`\\b${name}\\s*=\\s*(?:"([^"]*)"|'([^']*)'|([^\\s>]+))`, 'i').exec(tag)?.slice(1).find(value => value !== undefined);
    for (const match of clean.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script\s*>/gi)) {
      const src = attribute(match[1]!, 'src');
      if (src) dependency(src, path, true);
      else script(match[2]!, path);
    }
    for (const match of clean.matchAll(/<style\b[^>]*>([\s\S]*?)<\/style\s*>/gi)) css(match[1]!, path);
    for (const match of clean.matchAll(/<link\b([^>]*)>/gi)) {
      const rel = attribute(match[1]!, 'rel');
      const href = attribute(match[1]!, 'href');
      if (href && rel && /(?:stylesheet|preload|modulepreload)/i.test(rel)) dependency(href, path, true);
    }
    for (const match of clean.matchAll(/<(?:img|source|video|audio)\b([^>]*)>/gi)) {
      const src = attribute(match[1]!, 'src');
      if (src && !src.startsWith('{')) dependency(src, path, true);
    }
  };
  const visit = (path: string): void => {
    const key = record(path);
    if (visited.has(key)) return;
    visited.add(key);
    found.add(key);
    const extension = extname(path);
    if (!['.ts', '.tsx', '.js', '.jsx', '.mjs', '.cjs', '.astro', '.css', '.html'].includes(extension)) return;
    const text = readFileSync(path, 'utf8');
    if (extension === '.css') css(text, path);
    else if (extension === '.html') markup(text, path);
    else if (extension === '.astro') {
      const frontmatter = /^---\s*\r?\n([\s\S]*?)\r?\n---(?:\r?\n|$)/.exec(text);
      if (frontmatter) script(frontmatter[1]!, path);
      markup(frontmatter ? text.slice(frontmatter[0].length) : text, path);
    } else script(text, path);
  };
  for (const entry of roots) visit(localFile(resolve(repository, entry)));
  for (let index = 0; index < configEntries.length; index++) visit(configEntries[index]!);
  return [...found].sort();
}

/** Registry lists only renderer/spec roots and non-importable harness inputs; closure owns the rest. */
export function scopeInputs(scope: RendererScope, root = process.cwd()): string[] {
  root = realpathSync(root);
  const common = ['package.json', 'bun.lock', 'tsconfig.json', 'playwright.config.ts',
    'packages/design-tokens/package.json', 'packages/design-tokens/tokens.json', 'packages/design-tokens/generate.ts',
    'packages/design-tokens/src/tokens.ts', 'packages/design-tokens/src/tokens.css',
    'scripts/frontend/baseline-provenance.ts', 'scripts/frontend/scoped-inputs.ts', 'scripts/frontend/update-baselines.ts'];
  const storefrontConfig = ['web/storefront/package.json', 'web/storefront/tsconfig.json', 'web/storefront/astro.config.mjs'];
  const publicPages = ['index.astro', '[...slug].astro', 'products/index.astro', 'products/[slug].astro', '404.astro', '502.astro'];
  const commercePages = ['cart.astro', 'checkout.astro', 'account.astro', 'account/orders.astro', 'customer/verify.astro', 'order-complete.astro', 'purchase/api/[...operation].ts', 'products/[slug].astro'];
  // Both storefront fixtures build the real API and apply every migration. Keep this non-TS harness
  // conservative across runtime Go sources and SQL: later CMS function overrides need invalidation too.
  const reader = ['tests/fixtures/storefront_seed.sql', 'scripts/start_storefront_api_e2e.sh', 'scripts/seed_storefront.sh',
    'scripts/migrate.sh', 'scripts/migrate_river.sh', 'go.mod', 'go.sum',
    ...['internal', 'cmd/api', 'cmd/river-migrate'].flatMap(directory => filesIn(join(root, directory)))
      .filter(path => path.endsWith('.go') && !path.endsWith('_test.go')).map(path => relative(root, path)),
    ...filesIn(join(root, 'migrations')).filter(path => path.endsWith('.sql')).map(path => relative(root, path))];
  const registry: Record<RendererScope, string[]> = {
    gallery: ['web/admin/index.html', 'web/admin/package.json', 'web/admin/vite.config.ts', 'tests/frontend/gallery.spec.ts'],
    admin: ['web/admin/index.html', 'web/admin/package.json', 'web/admin/vite.config.ts', 'tests/frontend/admin.spec.ts'],
    'storefront-published': [...storefrontConfig, ...publicPages.map(page => `web/storefront/src/pages/${page}`),
      'tests/frontend/storefront.spec.ts', 'scripts/start_storefront_ssr.sh', ...reader],
    'storefront-commerce': [...storefrontConfig, ...commercePages.map(page => `web/storefront/src/pages/${page}`),
      'tests/frontend/storefront-commerce.spec.ts', 'tests/frontend/commerce-fixture.ts', 'scripts/start_storefront_ssr.sh', ...reader],
    'real-backend': [...storefrontConfig, ...commercePages.map(page => `web/storefront/src/pages/${page}`),
      'tests/frontend/real-backend.spec.ts', 'scripts/frontend/real-proxy.ts', 'scripts/frontend/start-real-backend.py',
      'scripts/e2e/seed.sql', 'scripts/e2e/purchase.py', 'scripts/ops_deploy.py',
      'scripts/start_storefront_ssr.sh', 'cmd/bootstrap-admin/main.go', ...reader],
  };
  if (!(scope in registry)) throw new Error(`Unknown renderer scope: ${scope}`);
  const assetDirectory = scope.startsWith('storefront-') || scope === 'real-backend' ? 'web/storefront/public' : 'web/admin/public';
  const assets = filesIn(join(root, assetDirectory)).map(path => relative(root, path));
  // Hash global authority/config files directly: following the Playwright writer import would collapse all scopes.
  for (const path of common) if (!existsSync(join(root, path))) throw new Error(`Missing common input: ${path}`);
  return [...new Set([...common, ...assets, ...graphInputFiles(registry[scope], root)])].sort();
}
