/** Source-independent packaging preflight. CI and copy-images consume the same active slot/static contract.
 * Depends on public manifests and locale dictionaries only; never reads private masters or changes dist. */
import { readdirSync, readFileSync, existsSync } from 'node:fs';
import { join, relative } from 'node:path';
import products from './src/data/products.json';
import digital from './digital-sources.json';
import details from './detail-sources.json';
import home from './home-media.json';
import about from './about-media.json';
import service from './service-media.json';
import contact from './contact-media.json';
import frozen from './design/collection-frozen.json';
import { slotInventory, staticImages, assertSlotReferences, digitalDelivery } from './image-plan';
import { imageBase, digitalAlt, widths } from './src/data/imagery';
import { dictionaries, productText, locales, type Locale } from './src/i18n';
import { orderedGallery } from './src/data/gallery';
export function htmlFiles(dir: string): string[] {
 return readdirSync(dir, {withFileTypes:true}).flatMap(e => e.isDirectory() ? htmlFiles(join(dir,e.name)) : e.name.endsWith('.html') ? [join(dir,e.name)] : []);
}
const escape = (s: string) => s.replaceAll('&','&amp;').replaceAll('"','&quot;').replaceAll('<','&lt;').replaceAll('>','&gt;');
// Includes every srcset candidate, data-* value and literal JS URL; gallery JS also derives mobile URLs.
export function imageReferences(html: string): Set<string> {
 const refs = new Set([...html.matchAll(/\/img\/[^\s"'<>`,;()\\]+?\.(?:webp|avif)/g)].map(m=>m[0]));
 for (const m of html.matchAll(/<a\b[^>]*>/g)) {
  const href=m[0].match(/\bhref="(\/img\/[^"]+\.webp)"/)?.[1];
  if (href && /\bdata-role="/.test(m[0]) && !/\bdata-fit="contain"/.test(m[0])) refs.add(href.replace(/\.webp$/, '-640.webp'));
 }
 return refs;
}
export function packagingGate(root: string) {
 const planned = new Set([
  ...products.flatMap(p => [...p.images.map(i=>i.src), ...p.size_chart]),
  ...[...digital, ...home, ...about, ...service, ...contact, ...frozen.assets].flatMap(i=>digitalDelivery(i).map(d=>d.src)),
  ...details.map(i=>i.src),
  ...Object.entries(slotInventory).flatMap(([page,slots])=>slots.flatMap(slot=>widths.flatMap(width=>['webp','avif'].map(format=>`${imageBase(`${page}/${slot}`)}-${width}.${format}`)))),
 ]);
 const cardSlugs = new Map<string, Set<string>>();
 for (const p of products) for (const { src } of p.images) {
  const names = cardSlugs.get(src) ?? new Set<string>();
  names.add(p.slug);
  cardSlugs.set(src, names);
 }
 const errors: string[] = []; let images=0;
 for (const file of htmlFiles(root)) {
  const route=relative(root,file), prefix=route.split('/')[0];
  const locale: Locale=locales.find(l=>l===prefix)||'en', t=dictionaries[locale];
  const html=readFileSync(file,'utf8');
  const localized=new Set([...Object.values(t), ...Object.values(productText[locale]).map(p=>p.name)].map(escape));
  for (const p of products) for (const [index] of orderedGallery(p.images).entries()) localized.add(escape(`${productText[locale][p.slug].name}: ${t.view} ${index + 1}`));
  for(const p of products) for(const src of p.size_chart) localized.add(escape(`${t.sizeChart}: ${p.style_codes.join(', ')}`));
  for (const match of html.matchAll(/<img\b[^>]*>/g)) {
   images++; const tag=match[0],src=tag.match(/\bsrc="([^"]+)"/)?.[1]||'',alt=tag.match(/\balt="([^"]*)"/)?.[1] ?? (/\salt(?:\s|\/?>)/.test(tag) ? '' : undefined);
   if (staticImages[src]) {
    const data=existsSync(join(root,src)) ? readFileSync(join(root,src),'utf8') : '';
    const {w,h}=staticImages[src];
    if (!data.includes(`width="${w}" height="${h}"`) || alt === undefined) errors.push(`${route}: invalid static asset ${src}`);
    continue;
   }
   if (!planned.has(src)) errors.push(`${route}: unplanned image ${src}`);
   // Frozen editorial images have decorative semantics; product/slot images require locale-bound copy.
   const decorative=[...service.filter(i=>i.kind==='catalog'),...home,...about,...frozen.assets,...contact.filter(i=>i.id!=='pattern')].some(i=>i.src===src)
    || /(?:^|\/)contact\/index.html$/.test(route) && digital.some(i=>i.src===src && i.wave==='W1' && ['apparel-59043','cotton430-hoodie-58065'].includes(i.slug));
   const translated=digitalAlt(src,locale);
   const cardAlt = translated && [...(cardSlugs.get(src) ?? [])].some(slug =>
    alt === escape(`${productText[locale][slug].name} — ${translated}`));
   if (alt===undefined || !(alt==='' && decorative) && !localized.has(alt) && !(translated && alt===escape(translated)) && !cardAlt) errors.push(`${route}: missing or unlocalized alt ${src}`);
  }
  // Every other image reference (srcset, href, data-*, and the -640 URLs gallery JS derives from thumbnail hrefs)
  // is fetched by the browser too; an <img src>-only gate misses them.
  for (const ref of imageReferences(html)) if (!planned.has(ref) && !staticImages[ref]) errors.push(`${route}: unplanned image reference ${ref}`);
  if (/(?:^|\/)products\/[^/]+\/index.html$/.test(route)) {
   try { assertSlotReferences(html,slotInventory.pdp.map(slot=>({id:`pdp/${slot}`})),'pdp'); } catch(e) { errors.push(`${route}: ${e}`); }
  }
 }
 if(errors.length) throw Error(errors.join('\n'));
 console.log(`PASS packaging contract: ${images} images, ${htmlFiles(root).length} HTML files; planned sources, localized alts and static dimensions.`);
 return images;
}
if(import.meta.main) packagingGate(process.argv[2]||process.env.APPAREL_DIST||'web/apparel/dist');
