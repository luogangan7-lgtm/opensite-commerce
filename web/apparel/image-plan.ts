/** Pure deploy selection contract; called by copy-images and CI. No IO, embedded review rows or private paths. */
import { basename } from 'node:path';
import { imageBase, publicId } from './src/data/imagery';
export interface Review { slug: string; role: string; path: string; sha256: string; w: number; h: number; pass: boolean }
interface Input { slots: Record<string, Record<string, string>>; review: Review[]; provenance: Record<string, { rights: string }>; published: string[] }
export interface Approved { slug: string; role: string; source: string; src: string; sha256: string; w: number; h: number; id?: string; rights?: string }
export function imagePlan({ slots, review, provenance, published }: Input) {
  const eligible = review.filter(row => row.pass === true && row.role !== 'dh-legacy' && !row.path.split(/[\\/]/).includes('dh-legacy') && basename(row.path) !== 'model_front.png');
  const checked = (row: Review, source: string, src: string): Approved => {
    if (!/^[a-z0-9_-]+\/[a-z0-9_-]+\.png$/.test(source) || !row.path.replaceAll('\\', '/').endsWith(`/${source}`) || !/^[a-f0-9]{64}$/.test(row.sha256) || !Number.isInteger(row.w) || !Number.isInteger(row.h) || row.w <= 0 || row.h <= 0) throw Error('Invalid reviewed source');
    return { slug: row.slug, role: row.role, source, src, sha256: row.sha256, w: row.w, h: row.h };
  };
  const slotReviews: { id: string; pass: boolean }[] = [];
  const illustrations = Object.entries(slots).flatMap(([page, entries]) => Object.entries(entries).flatMap(([slot, filename]) => {
    if (!/^[a-z]+$/.test(page) || !/^[a-z0-9_-]+$/.test(slot) || filename !== `illustrative_${page}_${slot}.png`) throw Error('Invalid slot source');
    const rows = review.filter(row => row.slug === '_site' && row.role === filename.slice(0, -4) && basename(row.path) === filename);
    if (rows.length !== 1) throw Error(`Missing or ambiguous review: ${page}/${slot}`);
    if (typeof rows[0].pass !== 'boolean') throw Error(`Invalid review flag: ${page}/${slot}`);
    slotReviews.push({ id: publicId(`${page}/${slot}`), pass: rows[0].pass });
    if (rows[0].pass === false) return [];
    if (!eligible.includes(rows[0])) throw Error(`Invalid passing review: ${page}/${slot}`);
    if (provenance?.[filename]?.rights !== 'ai-generated-illustrative') throw Error(`Missing provenance: ${page}/${slot}`);
    const id = publicId(`${page}/${slot}`);
    return [{ ...checked(rows[0], `_site/${filename}`, `${imageBase(id)}-1024.webp`), id, rights: 'ai-generated-illustrative' }];
  }));
  const packshots = eligible.filter(row => row.role === 'packshot' && basename(row.path) === 'packshot.png' && published.includes(row.slug)).map(row => {
    if (!/^[a-z0-9-]+$/.test(row.slug)) throw Error('Invalid published slug');
    return checked(row, `${row.slug}/packshot.png`, `/img/${row.slug}/packshot.webp`);
  }).sort((a, b) => a.slug.localeCompare(b.slug));
  if (new Set(packshots.map(row => row.slug)).size !== packshots.length) throw Error('Duplicate packshot review');
  return { slots: illustrations, slotReviews, packshots };
}

/** Packaging is the sole publication writer; omitted reviews remove the entire picture, never only src. */
export function publishSlotMarkup(html: string, slots: { id?: string }[]): string {
  const allowed = new Set(slots.map(slot => slot.id));
  return html.replace(/<picture\b[^>]*data-slot="([^"]+)"[^>]*>[\s\S]*?<\/picture>/g, (picture, id) => allowed.has(id) ? picture : '')
    .replace(/<link\b[^>]*rel="preload"[^>]*>/g, link => !link.includes('/img/site/') || slots.some(slot => slot.id && link.includes(imageBase(slot.id) + '-')) ? link : '');
}

/** Gate: every review-passing slot is referenced on its page; every excluded slot is absent. */
export function assertSlotReferences(html: string, slots: { id?: string }[], page: string): void {
  const expected = slots.filter(slot => slot.id?.startsWith(page + '/')).map(slot => slot.id!);
  const pictures = [...html.matchAll(/<picture\b[^>]*data-slot="([^"]+)"[^>]*>[\s\S]*?<\/picture>/g)];
  for (const id of expected) {
    const picture = pictures.find(row => row[1] === id)?.[0];
    if (!picture || !picture.includes(`<img `) || !picture.includes(`src="${imageBase(id)}-1024.webp"`)) throw Error(`missing slot or localized alt ${id}`);
  }
  for (const picture of pictures) if (picture[1].startsWith(page + '/') && !expected.includes(picture[1])) throw Error(`Unpublished slot: ${picture[1]}`);
}
