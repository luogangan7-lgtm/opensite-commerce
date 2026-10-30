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
