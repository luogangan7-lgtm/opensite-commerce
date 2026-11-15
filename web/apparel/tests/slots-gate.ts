/** CI output contract, independent of page implementation; deploy checks use the external slot inventory. */
import digital from '../digital-sources.json';
import frozen from '../design/collection-frozen.json';
import { createHash } from 'node:crypto';
import products from '../src/data/products.json';
import { assertSlotReferences, slotInventory } from '../image-plan';
import { readFileSync, existsSync } from 'node:fs';
import { join } from 'node:path';
const root = process.env.APPAREL_DIST || 'web/apparel/dist';
export const inventory = slotInventory;
const routes: Record<string, string[]> = { home: [''], collection: ['collections', 'collections/crew-fleece'], about: ['about'], oem: ['about'], pdp: ['products/crew-fleece-8900-32'], contact: ['contact'] };
// Raw CI artifacts retain every template so a later passing review needs only repackaging.
// Packaged output instead uses the complete review decision inventory, never a filename exception.
const manifestPath = join(root, 'deploy-manifest.json');
const manifest = existsSync(manifestPath) ? JSON.parse(readFileSync(manifestPath, 'utf8')) : undefined;
const ids = Object.entries(inventory).flatMap(([page, slots]) => slots.map(slot => `${page}/${slot}`));
const passing: string[] = manifest ? manifest.slotReviews.filter((row: { pass: boolean }) => row.pass === true).map((row: { id: string }) => row.id) : ids;
if (manifest && (JSON.stringify(manifest.slotReviews.map((row: { id: string }) => row.id).sort()) !== JSON.stringify([...ids].sort()) || JSON.stringify(manifest.slots.map((row: { id: string }) => row.id).sort()) !== JSON.stringify([...passing].sort()))) throw Error('Review inventory and published slots disagree');
const errors: string[] = [];
for (const asset of frozen.catalog) if (!digital.some(row => row.src === asset.src && row.sha256 === asset.sha256 && row.source === asset.source)) errors.push(`Collection catalog master not accepted: ${asset.src}`);
// Published collection derivatives are verified against the frozen export receipts.
for (const asset of [...frozen.assets, ...frozen.catalog]) for (const file of asset.derivatives) {
  const path = join(root, file.src);
  if (!existsSync(path) || createHash('sha256').update(readFileSync(path)).digest('hex') !== file.sha256 || file.sourceSha256 !== asset.sha256) errors.push(`Collection digest mismatch: ${file.src}`);
}

for (const locale of ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar']) {
  const collection = readFileSync(join(root, locale === 'en' ? '' : locale, 'collections/index.html'), 'utf8');
  for (const asset of frozen.assets) if (!collection.includes(`src="${asset.src}"`)) errors.push(`Collection missing frozen image: ${locale}/${asset.id}`);
  const dictionary = JSON.parse(readFileSync(`web/apparel/src/i18n/${locale}.json`, 'utf8'));
  // Active placements are shared with deployment; FD1 uses product-bound media.
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
// Product-bound FD1 output replaces the three generic craft slots for every localized PDP.
for (const locale of ['en', 'zh', 'es', 'fr', 'de', 'pt', 'ru', 'ar']) {
 const dictionary = JSON.parse(readFileSync(`web/apparel/src/i18n/${locale}.json`, 'utf8'));
 for (const product of products) {
  const html = readFileSync(join(root, locale === 'en' ? '' : locale, 'products', product.slug, 'index.html'), 'utf8');
  const whole = product.images.find(image => image.role === 'front')!;
  const detail = product.images.find(image => image.role === 'structure' && image.colour === whole.colour)!;
  for (const [role, image] of [['whole', whole], ['detail', detail]] as const) {
   const tag = [...html.matchAll(/<img\b[^>]*>/g)].find(match => match[0].includes(`data-pdp-${role}`))?.[0] || '';
   // CP-PDP places the whole garment in the LCP gallery; details remain deferred.
   const priority = role === 'whole' ? tag.includes('loading="eager"') && tag.includes('fetchpriority="high"') : tag.includes('loading="lazy"');
   if (!tag.includes(`src="${image.src}"`) || !priority || !tag.includes('decoding="async"')) errors.push(`${locale}/${product.slug}: missing own ${role} FIT media`);
  }
  if (!html.includes('data-pdp-media') || html.includes('data-slot="pdp/craft_')) errors.push(`${locale}/${product.slug}: FD1 media missing or retired craft slot present`);
 }
}
if (errors.length) throw Error(errors.join('\n'));
console.log(`PASS: ${passing.length} ${manifest ? 'review-passing published slots' : 'build templates; publication filtered at packaging'}, eight localized alts, responsive sources, geometry and home LCP priority.`);
