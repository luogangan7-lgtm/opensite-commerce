/** CI output contract, independent of page implementation; deploy checks use the external slot inventory. */
import { assertSlotReferences } from '../image-plan';
import { readFileSync, existsSync } from 'node:fs';
import { join } from 'node:path';
const root = process.env.APPAREL_DIST || 'web/apparel/dist';
export const inventory = {
  home: ['hero', 'line_collections', 'line_oem', 'process_pattern', 'process_qc', 'process_sewing', 'track_custom', 'track_stock'],
  collection: ['hero', 'tile_apparel', 'tile_pullover-hoodie', 'tile_cotton430-hoodie', 'tile_crew-fleece', 'tile_hoodie-set'],
  about: ['hero', 'founder', 'facility_inspection', 'facility_production', 'facility_warehouse'],
  oem: ['hero', 'fabric_swatches', 'trims'],
  pdp: ['craft_fleece', 'craft_stack', 'craft_stitching'],
  contact: ['banner'],
};
const routes: Record<string, string[]> = { home: [''], collection: ['collections', 'collections/crew-fleece'], about: ['about'], oem: ['about'], pdp: ['products/crew-fleece-8900-32'], contact: ['contact'] };
// Raw CI artifacts retain every template so a later passing review needs only repackaging.
// Packaged output instead uses the complete review decision inventory, never a filename exception.
const manifestPath = join(root, 'deploy-manifest.json');
const manifest = existsSync(manifestPath) ? JSON.parse(readFileSync(manifestPath, 'utf8')) : undefined;
const ids = Object.entries(inventory).flatMap(([page, slots]) => slots.map(slot => `${page}/${slot}`));
const passing: string[] = manifest ? manifest.slotReviews.filter((row: { pass: boolean }) => row.pass === true).map((row: { id: string }) => row.id) : ids;
if (manifest && (JSON.stringify(manifest.slotReviews.map((row: { id: string }) => row.id).sort()) !== JSON.stringify([...ids].sort()) || JSON.stringify(manifest.slots.map((row: { id: string }) => row.id).sort()) !== JSON.stringify([...passing].sort()))) throw Error('Review inventory and published slots disagree');
const errors: string[] = [];
for (const locale of ['en', 'es', 'fr', 'de', 'pt', 'ru', 'ar']) {
  const dictionary = JSON.parse(readFileSync(`web/apparel/src/i18n/${locale}.json`, 'utf8'));
  for (const [page, slots] of Object.entries(inventory)) for (const route of routes[page]) {
    const html = readFileSync(join(root, locale === 'en' ? '' : locale, route, 'index.html'), 'utf8');
    try { assertSlotReferences(html, slots.filter(slot => passing.includes(`${page}/${slot}`)).map(slot => ({ id: `${page}/${slot}` })), page); } catch (error) { errors.push(`${locale}/${route}: ${error}`); }
    for (const slot of slots.filter(slot => passing.includes(`${page}/${slot}`))) {
      const id = `${page}/${slot}`, alt = dictionary[`alt.${page}.${slot}`];
      const picture = [...html.matchAll(/<picture\b[^>]*>[\s\S]*?<\/picture>/g)].find(row => row[0].includes(`data-slot="${id}"`))?.[0];
      if (!picture || !alt || !picture.includes(`alt="${alt.replaceAll('&', '&amp;').replaceAll('"', '&quot;')}"`)) { errors.push(`${locale}/${route}: missing slot or localized alt ${id}`); continue; }
      for (const format of ['avif', 'webp']) for (const width of [640, 1024, 1600]) if (!picture.includes(`-${width}.${format} ${width}w`)) errors.push(`${id}: missing ${width} ${format}`);
      if (!/\bsizes="[^"]+"/.test(picture) || !/\bwidth="\d+" height="\d+"/.test(picture)) errors.push(`${id}: missing geometry`);
      if (id === 'home/hero') {
        if (picture.includes('loading="lazy"') || !picture.includes('fetchpriority="high"') || !/<link\b[^>]*rel="preload"[^>]*as="image"[^>]*imagesrcset="[^"]+"[^>]*imagesizes="[^"]+"/.test(html)) errors.push(`${id}: LCP priority/preload`);
      } else if (!picture.includes('loading="lazy"') || !picture.includes('decoding="async"')) errors.push(`${id}: missing lazy/async`);
    }
  }
}
if (errors.length) throw Error(errors.join('\n'));
console.log(`PASS: ${passing.length} ${manifest ? 'review-passing published slots' : 'build templates; publication filtered at packaging'}, seven localized alts, responsive sources, geometry and home LCP priority.`);
