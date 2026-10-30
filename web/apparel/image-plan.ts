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
  const illustrations = Object.entries(slots).flatMap(([page, entries]) => Object.entries(entries).map(([slot, filename]) => {
    if (!/^[a-z]+$/.test(page) || !/^[a-z0-9_-]+$/.test(slot) || filename !== `illustrative_${page}_${slot}.png`) throw Error('Invalid slot source');
    if (provenance?.[filename]?.rights !== 'ai-generated-illustrative') throw Error(`Missing provenance: ${page}/${slot}`);
    const rows = eligible.filter(row => row.slug === '_site' && row.role === filename.slice(0, -4) && basename(row.path) === filename);
    if (rows.length !== 1) throw Error(`Missing or ambiguous passing review: ${page}/${slot}`);
    const id = publicId(`${page}/${slot}`);
    return { ...checked(rows[0], `_site/${filename}`, `${imageBase(id)}-1024.webp`), id, rights: 'ai-generated-illustrative' };
  }));
  const packshots = eligible.filter(row => row.role === 'packshot' && basename(row.path) === 'packshot.png' && published.includes(row.slug)).map(row => {
    if (!/^[a-z0-9-]+$/.test(row.slug)) throw Error('Invalid published slug');
    return checked(row, `${row.slug}/packshot.png`, `/img/${row.slug}/packshot.webp`);
  }).sort((a, b) => a.slug.localeCompare(b.slug));
  if (new Set(packshots.map(row => row.slug)).size !== packshots.length) throw Error('Duplicate packshot review');
  return { slots: illustrations, packshots };
}
