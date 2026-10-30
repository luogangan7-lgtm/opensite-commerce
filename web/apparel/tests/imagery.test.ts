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
