/** Pure deploy selection contract; called by copy-images and CI. No IO, embedded review rows or private paths. */
import { basename, dirname } from 'node:path';
import { imageBase, publicId } from './src/data/imagery';
export interface Review { slug: string; role: string; path: string; sha256: string; w: number; h: number; pass: boolean }
export interface DigitalSource { key: string; slug: string; role: string; source: string; src: string; sha256: string; wave: string; acceptance: string; backReference?: string; reviewEvidence?: string }
export interface DetailSource { slug: string; role: 'fabric' | 'structure'; colour: string; source: string; src: string; sha256: string; acceptance: string; sourceSha256: string }
export interface DetailLedger { style_slug: string; source_slug: string; colour: string; logical_slot: string; asset_path: string; asset_sha256: string; source_sha256: string; accepted_evidence: string }
interface Input { detailLedger?: DetailLedger[]; details?: DetailSource[]; digital?: DigitalSource[]; slots: Record<string, Record<string, string>>; review: Review[]; provenance: Record<string, { rights: string }>; published: string[] }
export interface Approved { slug: string; role: string; source: string; src: string; sha256: string; w: number; h: number; id?: string; rights?: string }
export function imagePlan({ slots, review, provenance, published, digital = [], details = [], detailLedger = [] }: Input) {
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
  const models = digital.map(row => {
    if (publicId(row.key.split(/--|__/)[0]) !== row.slug || !published.includes(row.slug) || !/^[a-z0-9-]+$/.test(row.slug) || !/^digital-(?:front|turn|back|model-\d+)$/.test(row.role) || row.src !== `/img/${row.slug}/${row.role}.webp` || !/^[a-f0-9]{64}$/.test(row.sha256) || row.source.startsWith('/') || row.source.split(/[\\/]/).some(p => p === '..') || !/\.(?:png|raw)$/.test(row.source)) throw Error('Invalid digital model source or catalog identity');
    return row;
  });
  if (new Set(models.map(row => row.src)).size !== models.length) throw Error('Duplicate digital model view');
  const detailImages = details.map(row => {
    assertDetailLedger(row, detailLedger);
    if (!published.includes(row.slug) || !/^[a-z0-9-]+$/.test(row.slug) || !['fabric', 'structure'].includes(row.role) || !row.colour || publicId(basename(row.source) === 'final.png' ? basename(dirname(row.source)) + '.png' : basename(row.source)) !== `${row.slug}--${row.role}.png` || row.src !== `/img/${row.slug}/${row.role}.webp` || !/^[a-f0-9]{64}$/.test(row.sha256) || !/^[a-f0-9]{64}$/.test(row.sourceSha256) || row.source.startsWith('/') || row.source.split(/[\\/]/).some(p => p === '..') || !row.source.endsWith('.png') || !row.acceptance) throw Error('Invalid detail source or catalog identity');
    return row;
  });
  if (new Set([...models, ...detailImages].map(row => row.src)).size !== models.length + detailImages.length) throw Error('Duplicate image view');
  return { slots: illustrations, slotReviews, packshots, digital: models, details: detailImages };
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

/** A changed source is a publication failure, even if it passed an earlier visual review. */
export function assertDigitalDigest(image: DigitalSource, actual: string): void {
  if (actual !== image.sha256) throw Error(`Digital model source changed: ${image.key}`);
}

/** Explicit single-image independent review format; binds final.png, identity and PASS together. */
export function assertSingleRelease(report: string, key: string, sha: string): void {
  if (!/^结论：PASS。/m.test(report) || !report.includes(`record：${key}；`) ||
      !report.split('\n').some((line, i, lines) => line.endsWith('/final.png') && lines[i + 1] === `SHA256：${sha}`)) throw Error(`Single-image release not PASS: ${key}`);
}

/** Detail publication uses the exact independently accepted bytes, never a similarly named file. */
export function assertDetailLedger(image: DetailSource, ledger: DetailLedger[]): DetailLedger {
  const matches = ledger.filter(row => publicId(row.style_slug) === image.slug && row.logical_slot === `pdp.${image.role}`);
  if (matches.length !== 1) throw Error(`Missing or ambiguous detail ledger: ${image.slug}`);
  const row = matches[0];
  const relative = (path: string) => path.split('/work/apparel-b2b/')[1];
  if (publicId(row.source_slug) !== image.slug || row.colour !== image.colour || row.asset_sha256 !== image.sha256 || row.source_sha256 !== image.sourceSha256 || relative(row.asset_path) !== image.source || relative(row.accepted_evidence) !== image.acceptance || !/^[a-f0-9]{64}$/.test(row.asset_sha256) || !/^[a-f0-9]{64}$/.test(row.source_sha256)) throw Error(`Detail ledger binding mismatch: ${image.slug}`);
  return row;
}
export function assertDetailDigest(image: DetailSource, actual: string, ledger: DetailLedger[]): void {
  const accepted = assertDetailLedger(image, ledger);
  if (actual !== accepted.asset_sha256) throw Error(`Detail source changed from ledger: ${image.slug}/${image.role}`);
}

/** G/C native-pixel gate shared by accepted G packaging and ordinary C fallback copying. */
export function imageDimensions(role: string | undefined, metadata: {width?: number; height?: number}) {
  if (!metadata.width || !metadata.height) throw Error('Missing image geometry');
  if (['fabric','structure','print'].includes(role || '') && (metadata.width !== 1200 || metadata.height !== 1200)) throw Error('G/C detail requires native 1200×1200 pixels');
  return {w:metadata.width, h:metadata.height};
}
