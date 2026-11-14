import c21 from './accepted-c21.json';
/** CI-only deploy-plan contract; synthetic review rows are created in memory, never image generation. */
import { test, expect } from 'bun:test';
const load = () => import('../image-plan');
function fixture() {
  const row = (slug: string, role: string, file: string, pass = true) => ({ slug, role, path: `/fixture/hd/${slug}/${file}`, pass, sha256: 'a'.repeat(64), w: 1536, h: 1024 });
  return {
    slots: { home: { hero: 'illustrative_home_hero.png' } },
    review: [row('_site', 'illustrative_home_hero', 'illustrative_home_hero.png'), row('approved-style', 'packshot', 'packshot.png'), row('pending-style', 'packshot', 'packshot.png', false), row('forbidden-legacy', 'dh-legacy', 'packshot.png'), row('forbidden-model', 'packshot', 'model_front.png')],
    provenance: { 'illustrative_home_hero.png': { rights: 'ai-generated-illustrative' } },
    published: ['approved-style', 'pending-style', 'forbidden-legacy', 'forbidden-model'],
  };
}
test('manifest uses exactly approved packshots, and responds to a later review batch', async () => {
  const { imagePlan } = await load(), input = fixture();
  expect(imagePlan(input).packshots.map((p: { slug: string }) => p.slug)).toEqual(['approved-style']);
  input.review[2].pass = true;
  expect(imagePlan(input).packshots.map((p: { slug: string }) => p.slug).sort()).toEqual(['approved-style', 'pending-style']);
  expect(JSON.stringify(imagePlan(input))).not.toMatch(/dh-legacy|model_front|\/fixture/);
});
test('missing provenance prevents publication; failed reviews exclude slots', async () => {
  const { imagePlan } = await load(), input = fixture();
  expect(() => imagePlan({ ...input, provenance: {} })).toThrow(/provenance/i);
  input.review[0].pass = false;
  expect(imagePlan(input).slots).toEqual([]);
});
test('unpublished packshots and escaping or ambiguous source rows fail closed', async () => {
  const { imagePlan } = await load(), input = fixture();
  expect(imagePlan({ ...input, published: [] }).packshots).toEqual([]);
  expect(() => imagePlan({ ...input, slots: { home: { hero: '../escape.png' } } })).toThrow();
  input.review.push({ ...input.review[1] });
  expect(() => imagePlan(input)).toThrow(/duplicate|ambiguous/i);
});

// Fixture JSONL only: the external review file is never edited or embedded.
test('a rejected contact slot is absent, then returns after a fixture review flip', async () => {
  const api: Record<string, any> = await load(), input = fixture();
  input.slots = { ...input.slots, contact: { banner: 'illustrative_contact_banner.png' } } as typeof input.slots;
  input.provenance['illustrative_contact_banner.png' as keyof typeof input.provenance] = { rights: 'ai-generated-illustrative' };
  const reviewJSONL = [...input.review, { ...input.review[0], role: 'illustrative_contact_banner', path: '/fixture/hd/_site/illustrative_contact_banner.png', pass: false }].map(row => JSON.stringify(row)).join('\n');
  input.review = reviewJSONL.split('\n').map(line => JSON.parse(line));
  const html = '<div class="contact-banner"><picture data-slot="contact/banner"><img src="/img/site/illustrative_contact_banner-1024.webp" alt="Fixture"></picture></div>';
  expect(api.imagePlan(input).slots.map((s: { id: string }) => s.id)).toEqual(['home/hero']);
  expect(api.publishSlotMarkup(html, api.imagePlan(input).slots)).toBe('<div class="contact-banner"></div>');
  input.review.at(-1)!.pass = true;
  expect(api.publishSlotMarkup(html, api.imagePlan(input).slots)).toBe(html);
  // Selection is generic: no contact filename exception.
  input.review[0].pass = false;
  expect(api.imagePlan(input).slots.map((s: { id: string }) => s.id)).toEqual(['contact/banner']);
});

test('the real slot gate rejects a missing passing slot in a disposable built fixture', async () => {
  const { cpSync, mkdtempSync, readFileSync, writeFileSync, rmSync } = await import('node:fs');
  const { tmpdir } = await import('node:os'), { join } = await import('node:path');
  const dir = mkdtempSync(join(tmpdir(), 'apparel-r4b-slot-'));
  try {
    cpSync('web/apparel/dist', dir, { recursive: true });
    const file = join(dir, 'index.html');
    writeFileSync(file, readFileSync(file, 'utf8').replace(/<picture\b[^>]*data-slot="home\/hero"[^>]*>[\s\S]*?<\/picture>/, ''));
    const red = Bun.spawnSync(['bun', 'web/apparel/tests/slots-gate.ts'], { env: { ...process.env, APPAREL_DIST: dir } });
    expect(red.exitCode).not.toBe(0);
    expect(red.stderr.toString()).toContain('missing slot or localized alt home/hero');
    console.log('RED proven: missing passing home/hero rejected by real HTML gate');
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test('review-filtered HTML gate rejects missing passing and leaked failing slots', async () => {
  const api: Record<string, any> = await load(), input = fixture();
  const plan = api.imagePlan(input);
  expect(() => api.assertSlotReferences('', plan.slots, 'home')).toThrow(/missing slot.*home\/hero/);
  const html = '<picture data-slot="home/hero"><img src="/img/site/illustrative_home_hero-1024.webp"></picture>';
  expect(() => api.assertSlotReferences(html, plan.slots, 'home')).not.toThrow();
  input.review[0].pass = false;
  const rejected = api.imagePlan(input);
  expect(() => api.assertSlotReferences(html, rejected.slots, 'home')).toThrow(/Unpublished slot/);
  const clean = api.publishSlotMarkup(html, rejected.slots);
  expect(clean).not.toContain('<img');
  expect(() => api.assertSlotReferences(clean, rejected.slots, 'home')).not.toThrow();
  console.log('RED proven: missing passing slot and leaked rejected slot; restored fixture GREEN');
});

test('the real HTML gate accepts a filtered fixture and a later passing review without code changes', async () => {
  const api: Record<string, any> = await load();
  const { dictionaries } = await import('../src/i18n');
  const { cpSync, mkdtempSync, readFileSync, writeFileSync, rmSync, readdirSync } = await import('node:fs');
  const { tmpdir } = await import('node:os'), { join } = await import('node:path');
  const dir = mkdtempSync(join(tmpdir(), 'apparel-r4b-filtered-'));
  const slots: Record<string, Record<string, string>> = {};
  for (const key of Object.keys(dictionaries.en).filter(key => key.startsWith('alt.'))) {
    const [page, slot] = key.slice(4).split('.');
    (slots[page] ||= {})[slot] = `illustrative_${page}_${slot}.png`;
  }
  const filenames = Object.values(slots).flatMap(Object.values);
  // A copied synthetic JSONL fixture exercises the exact review parser and public HTML gate.
  const fixtureJSONL = filenames.map(name => JSON.stringify({ slug: '_site', role: name.slice(0, -4), path: `/fixture/hd/_site/${name}`, sha256: 'a'.repeat(64), w: 1536, h: 1024, pass: name !== slots.contact.banner })).join('\n');
  const review = fixtureJSONL.split('\n').map(line => JSON.parse(line));
  const input = { slots, review, provenance: Object.fromEntries(filenames.map(name => [name, { rights: 'ai-generated-illustrative' }])), published: [] };
  const gate = () => Bun.spawnSync(['bun', 'web/apparel/tests/slots-gate.ts'], { env: { ...process.env, APPAREL_DIST: dir } });
  const writeFixture = () => {
    cpSync('web/apparel/dist', dir, { recursive: true });
    const plan = api.imagePlan(input);
    const visit = (path: string) => { for (const entry of readdirSync(path, { withFileTypes: true })) { const file = join(path, entry.name); if (entry.isDirectory()) visit(file); else if (entry.name.endsWith('.html')) writeFileSync(file, api.publishSlotMarkup(readFileSync(file, 'utf8'), plan.slots)); } };
    visit(dir);
    writeFileSync(join(dir, 'deploy-manifest.json'), JSON.stringify(plan));
  };
  try {
    writeFixture();
    expect(gate().exitCode).toBe(0);
    expect(readFileSync(join(dir, 'contact/index.html'), 'utf8')).not.toContain('data-slot="contact/banner"');
    const file = join(dir, 'index.html'), original = readFileSync(file, 'utf8');
    writeFileSync(file, original.replace(/<picture\b[^>]*data-slot="home\/hero"[^>]*>[\s\S]*?<\/picture>/, ''));
    expect(gate().exitCode).not.toBe(0);
    writeFileSync(file, original);
    review.find(row => row.role === 'illustrative_contact_banner')!.pass = true;
    writeFixture();
    expect(gate().exitCode).toBe(0);
    expect(readFileSync(join(dir, 'contact/index.html'), 'utf8')).toContain('data-slot="contact/banner"');
    console.log('PASS: real gate 25 passing slots, missing-slot RED, fixture review flip restores 26');
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

// Public metadata and private review receipts must remain a one-to-one catalog join.
import digital from '../digital-sources.json';
import originalRegenerated from './accepted-regenerated.json';
import r5 from './accepted-r5.json';
const regenerated = [...originalRegenerated, ...r5];
import approval from '../catalog-approval.json';
import products from '../src/data/products.json';
import { digitalImages, digitalAlt, digitalLabel, publicId } from '../src/data/imagery';
import { locales } from '../src/i18n';
import { existsSync, readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { resolve, join } from 'node:path';
import { privateSourceRoot, assertPrivateOriginalInputs } from './private-originals';
const digest = (bytes: Uint8Array) => createHash('sha256').update(bytes).digest('hex');
// Private originals are optional only when not explicitly selected. Tracked code under work/ is not a bundle.
const selectedSourceRoot = privateSourceRoot(process.env);
const sourceRoot = selectedSourceRoot ?? '';
// FIN adds one W2 front (70179 model-03); its five G structures are outside this digital-model inventory.
test('115 licensed catalog identities; all declared digital models join by exact style and role', async () => {
  const { imagePlan } = await load();
  expect(approval.products.length).toBe(115);
  expect(products.map(p => p.slug).sort()).toEqual(approval.products.map(p => publicId(p.slug)).sort());
  for (const p of approval.products) expect(['ok','owner-licensed']).toContain(p.rights);
  expect(digital.length).toBe(digitalImages.length);
  expect(digital.filter(p => p.wave === 'W3').length).toBe(digitalImages.filter(p => p.wave === 'W3').length);
  expect(digital.filter(p => p.wave !== 'W3').length).toBe(digitalImages.filter(p => p.wave !== 'W3').length);
  const plan = imagePlan({ ...fixture(), published: products.map(p => p.slug), digital });
  expect(plan.digital.length).toBe(digital.length);
  expect(new Set(digital.map(p => p.key)).size).toBe(digital.length);
  expect(new Set(digital.map(p => p.src)).size).toBe(digital.length);
  expect(digitalImages.map(p => p.src).sort()).toEqual(digital.map(p => p.src).sort());
  for (const image of digital) {
    expect(publicId(image.key.split(/--|__/)[0])).toBe(image.slug);
    expect(products.find(p => p.slug === image.slug)?.images.filter(i => i.src === image.src).length).toBe(1);
    expect(digitalImages.find(p => p.src === image.src)?.slug).toBe(image.slug);
  }
  expect(digital.find(p => p.key === 'cotton430-hoodie-58065__model-03')?.sha256).toBe(r5.find(row => row.slug === 'cotton430-hoodie-58065')!.sha256);
  expect(digital.find(p => p.key === 'crew-fleece-8900-46__model-03')?.sha256).toBe('cd78398f7f4865b95113648ea6c6b8af2e8d91d54bb3d80c8e8a0dfb546fc8a0');
});
test('every digital model has eight colour, garment, angle and disclosure descriptions', () => {
  for (const image of digitalImages) {
    const alts = locales.map(locale => digitalAlt(image.src, locale));
    expect(alts.every(alt => typeof alt === 'string' && alt.split(';').length === 3 && !alt.includes('undefined'))).toBe(true);
    expect(new Set(alts).size).toBe(8);
    for (const locale of locales) expect(digitalAlt(image.src, locale)).toContain(digitalLabel(locale));
    if (image.wave === 'W3') expect(['turn','back']).toContain(image.view);
  }
});
test('changed bytes, wrong style, traversal and duplicate views fail closed then recover', async () => {
  const { imagePlan, assertDigitalDigest } = await load();
  const input = { ...fixture(), published: products.map(p => p.slug), digital: [digital[0]] };
  expect(() => assertDigitalDigest(digital[0], '0'.repeat(64))).toThrow('source changed');
  expect(() => assertDigitalDigest(digital[0], digital[0].sha256)).not.toThrow();
  for (const patch of [{ slug: 'unknown-style' }, { source: '../escaped.png' }, { src: '/img/wrong/front.webp' }, { role: 'packshot' }]) expect(() => imagePlan({ ...input, digital: [{ ...digital[0], ...patch }] })).toThrow();
  expect(() => imagePlan({ ...input, digital: [digital[0], digital[0]] })).toThrow('Duplicate');
  expect(imagePlan(input).digital.length).toBe(1);
});
test.skipIf(selectedSourceRoot === undefined)('local originals: 217 W3 and 536 W1/W2 hashes match accepted sources and current catalog', async () => {
  const { assertDigitalDigest, assertSingleRelease } = await load();
  assertPrivateOriginalInputs(sourceRoot, digital, approval.sha256);
  const catalogPath = resolve(sourceRoot, '../../docs/apparel-b2b/catalog.json');
  expect(digest(readFileSync(catalogPath))).toBe(approval.sha256);
  for (const image of digital) {
    const source = join(sourceRoot, image.source);
    expect(existsSync(source)).toBe(true);
    assertDigitalDigest(image, digest(readFileSync(source)));
    const receipt = readFileSync(join(sourceRoot, image.acceptance), 'utf8');
    // W3 tables have mixed headers and one final.png path; SHA and full source are authoritative.
    expect(receipt).toContain(image.sha256);
    const replacement = regenerated.find(row => row.src === image.src);
    if (replacement) expect(receipt).toContain(`${replacement.exactId}\t${image.sha256}\tPASS`);
    else expect(receipt).toContain(image.wave === 'W3' ? source : image.key);
    if (image.acceptance.endsWith('/r5-one-visual-20261107/REPORT.md')) assertSingleRelease(receipt, image.key, image.sha256);
    if (image.wave === 'W3') expect(image.source).toMatch(/\.(?:png|raw)$/);
  }
});

test('eight-language PDP includes primary accepted slots and selectable other-colour fronts with localized alt', () => {
  const escape = (text: string) => text.replaceAll('&', '&amp;').replaceAll('"', '&quot;').replaceAll("'", '&#39;').replaceAll('<', '&lt;').replaceAll('>', '&gt;');
  for (const locale of locales) for (const product of products) {
    const html = readFileSync(join('web/apparel/dist', locale === 'en' ? '' : locale, 'products', product.slug, 'index.html'), 'utf8');
    expect(html).toContain(digitalLabel(locale));
    const primary = product.images.find(image => image.role === 'front')?.colour;
    const seen = new Set<string>();
    for (const image of product.images) {
      const key = `${image.colour}:${image.role}`;
      const accepted = !!image.colour && ['front','turn','back','garment','print','structure','fabric'].includes(image.role) &&
        (image.colour === primary || image.role === 'front') && (image.role === 'print' || !seen.has(key));
      if (!accepted) { expect(html).not.toContain(`href="${image.src}"`); continue; }
      seen.add(key);
      expect(html).toContain(`href="${image.src}"`);
      const alt = digitalAlt(image.src, locale);
      if (alt) expect(html).toContain(`alt="${escape(alt)}"`);
    }
  }
});

test('public imagery metadata loads under the Node ESM runtime used by Playwright', () => {
  const result = Bun.spawnSync(['node', '--experimental-strip-types', '--input-type=module', '-e', `import('./web/apparel/src/data/imagery.ts').then(m => { if (m.digitalImages.length !== ${digital.length}) process.exit(1); })`]);
  expect(result.exitCode).toBe(0);
});

// Regression: every accepted W3 slot, including nested final.png, must be connected.
test.skipIf(selectedSourceRoot === undefined)('historical W3 tables plus accepted replacements resolve to the derived current slot count', () => {
  const { readdirSync } = require('node:fs');
  const fleet = join(sourceRoot, 'fleet-20261104');
  const accepted = new Map<string, string>();
  for (const dir of readdirSync(fleet).filter((name: string) => name.startsWith('w3-fin'))) {
    const file = join(fleet, dir, 'accepted.tsv');
    if (!existsSync(file)) continue;
    for (const line of readFileSync(file, 'utf8').split(/\r?\n/)) {
      const [, path, sha] = line.split('\t');
      if (!sha || !/^[a-f0-9]{64}$/.test(sha)) continue;
      const name = path.split('/').at(-1)!;
      const key = name === 'final.png' ? path.split('/').at(-2)! : name.replace(/\.png$/, '');
      if (accepted.has(key)) expect(accepted.get(key)).toBe(sha);
      accepted.set(key, sha);
      expect(digest(readFileSync(path))).toBe(sha);
    }
  }
  expect(accepted.size).toBe(154);
  for (const row of regenerated.filter(row => row.view !== 'front')) accepted.set(`${row.slug}--${row.view}`, row.sha256);
  expect(accepted.size).toBe(217);
  for (const row of c21.filter(row => row.view !== 'fabric')) accepted.set(`${row.slug}--${row.view}`, row.sha256);
  expect(accepted.size).toBe(217 + c21.filter(row => row.view !== 'fabric' && !row.replaced).length);
  expect(digital.filter(row => row.wave === 'W3').map(row => row.key).sort()).toEqual([...accepted.keys()].sort());
  for (const [key, sha] of accepted) expect(digital.find(row => row.key === key)?.sha256).toBe(sha);
});
test('back descriptions name the back angle in all eight languages without invented print details', () => {
  const backs = ['back view', '背面', 'vista posterior', 'vue de dos', 'Rückansicht', 'vista de costas', 'вид сзади', 'منظر خلفي'];
  for (const image of digitalImages.filter(row => row.view === 'back')) {
    expect(locales.map(locale => digitalAlt(image.src, locale)!.split('; ').at(-1)).sort()).toEqual([...backs].sort());
  }
});

test('all back sources distinguish same-colour, owner plain and colour-swapped references', () => {
  const backs = digital.filter(row => row.role === 'digital-back');
  expect(backs.filter(row => row.backReference === 'same-color').length).toBe(59 + c21.filter(r=>r.view==='back' && !r.replaced).length);
  expect(backs.filter(row => row.backReference === 'owner-plain').length).toBe(45);
  expect(backs.filter(row => row.backReference === 'other-color-ink').length).toBe(4);
  for (const row of backs) expect(['same-color', 'owner-plain', 'other-color-ink']).toContain(row.backReference);
});

// Synthetic owned bundle proves explicit selection cannot mask missing or corrupt private data.
test('private originals require explicit selection; declared incomplete or changed bundles fail closed', async () => {
  const { mkdirSync, mkdtempSync, writeFileSync, rmSync } = await import('node:fs');
  const dir = mkdtempSync(join('work', 'imagery-bundle-test-'));
  const root = join(dir, 'work/apparel-b2b'), catalog = resolve(root, '../../docs/apparel-b2b/catalog.json');
  const source = join(root, 'source.png'), accepted = join(root, 'accepted.tsv');
  const image = { source: 'source.png', acceptance: 'accepted.tsv', sha256: digest(Buffer.from('owned pixels')) };
  try {
    mkdirSync(join(root, 'fleet-20261104/sender-g'), { recursive: true });
    // This code-only prefix used to enable private tests just by existing.
    expect(existsSync(root)).toBe(true);
    expect(privateSourceRoot({})).toBeUndefined();
    expect(privateSourceRoot({ APPAREL_SOURCE_DIR: root })).toBe(resolve(root));
    expect(() => privateSourceRoot({ APPAREL_SOURCE_DIR: '' })).toThrow('explicitly');
    expect(() => assertPrivateOriginalInputs(root, [image], digest(Buffer.from('{}')))).toThrow('catalog.json'); // missing catalog
    mkdirSync(join(dir, 'docs/apparel-b2b'), { recursive: true });
    writeFileSync(catalog, '{}');
    expect(() => assertPrivateOriginalInputs(root, [image], digest(Buffer.from('{}')))).toThrow('source.png'); // missing source
    writeFileSync(source, 'owned pixels');
    expect(() => assertPrivateOriginalInputs(root, [image], digest(Buffer.from('{}')))).toThrow('accepted.tsv'); // missing accepted
    writeFileSync(accepted, image.sha256);
    expect(() => assertPrivateOriginalInputs(root, [image], digest(Buffer.from('{}')))).not.toThrow();
    writeFileSync(source, 'changed pixels');
    expect(() => assertPrivateOriginalInputs(root, [image], digest(Buffer.from('{}')))).toThrow('source SHA');
    writeFileSync(source, 'owned pixels'); writeFileSync(catalog, '{"changed":true}');
    expect(() => assertPrivateOriginalInputs(root, [image], digest(Buffer.from('{}')))).toThrow('catalog SHA');
    writeFileSync(catalog, '{}');
    expect(() => assertPrivateOriginalInputs(root, [image], digest(Buffer.from('{}')))).not.toThrow();
  } finally { rmSync(dir, { recursive: true, force: true }); }
});
