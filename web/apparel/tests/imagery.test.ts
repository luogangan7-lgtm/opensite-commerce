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
test('missing provenance or failed slot review prevents publication', async () => {
  const { imagePlan } = await load(), input = fixture();
  expect(() => imagePlan({ ...input, provenance: {} })).toThrow(/provenance/i);
  input.review[0].pass = false;
  expect(() => imagePlan(input)).toThrow(/review/i);
});
test('unpublished packshots and escaping or ambiguous source rows fail closed', async () => {
  const { imagePlan } = await load(), input = fixture();
  expect(imagePlan({ ...input, published: [] }).packshots).toEqual([]);
  expect(() => imagePlan({ ...input, slots: { home: { hero: '../escape.png' } } })).toThrow();
  input.review.push({ ...input.review[1] });
  expect(() => imagePlan(input)).toThrow(/duplicate|ambiguous/i);
});
