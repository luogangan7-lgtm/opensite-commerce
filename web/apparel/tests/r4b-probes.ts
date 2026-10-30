/** Isolated CI-only round 4b fault probes; never publish this branch. */
import { imagePlan, publishSlotMarkup, assertSlotReferences } from '../image-plan';
const review = ['home_hero', 'contact_banner'].map(name => ({ slug: '_site', role: `illustrative_${name}`, path: `/fixture/hd/_site/illustrative_${name}.png`, sha256: 'a'.repeat(64), w: 1536, h: 1024, pass: name === 'home_hero' }));
const plan = imagePlan({ slots: { home: { hero: 'illustrative_home_hero.png' }, contact: { banner: 'illustrative_contact_banner.png' } }, review, provenance: Object.fromEntries(review.map(row => [row.role + '.png', { rights: 'ai-generated-illustrative' }])), published: [] });
const home = '<picture data-slot="home/hero"><img src="/img/site/illustrative_home_hero-1024.webp"></picture>';
const contact = '<picture data-slot="contact/banner"><img src="/img/site/illustrative_contact_banner-1024.webp"></picture>';
function red(name: string, html: string, page: string, message: string) {
  try { assertSlotReferences(html, plan.slots, page); } catch (error) {
    if (!(error instanceof Error) || !error.message.includes(message)) throw error;
    console.log(`ISOLATED round4b ${name} RED proven: ${error.message}`); return;
  }
  throw Error(`Gate missed ${name}`);
}
red('missing-passing-slot', '', 'home', 'missing slot');
red('rejected-slot-leak', home + contact, 'contact', 'Unpublished slot');
const restored = publishSlotMarkup(home + contact, plan.slots);
assertSlotReferences(restored, plan.slots, 'home');
assertSlotReferences(restored, plan.slots, 'contact');
if (restored !== home) throw Error('Restored fixture is not clean');
console.log('ISOLATED round4b restored fixture GREEN');
