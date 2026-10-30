/** Deploy packaging only: exact green artifact + freshly reviewed sources -> isolated output, never a build. */
import { copyFileSync, mkdirSync, readFileSync, writeFileSync, readdirSync, existsSync, realpathSync } from 'node:fs';
import { dirname, resolve, join, relative } from 'node:path';
import { createRequire } from 'node:module';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import products from './src/data/products.json';
import { imagePlan, type Review, type Approved } from './image-plan';
import { widths, imageBase } from './src/data/imagery';
const sharp = createRequire(import.meta.resolve('astro/package.json'))('sharp');
const common = execFileSync('git', ['rev-parse', '--git-common-dir'], { encoding: 'utf8' }).trim();
const sourceRoot = process.env.APPAREL_SOURCE_DIR || resolve(dirname(resolve(common)), 'work/apparel-b2b');
const hd = join(sourceRoot, 'hd'), destination = resolve(process.argv[2] || '');
if (!process.argv[2] || !existsSync(join(destination, 'index.html')) || existsSync(join(destination, 'img')) || existsSync(join(destination, 'deploy-manifest.json'))) throw Error('Pass a fresh downloaded apparel-dist directory');
const json = (file: string) => JSON.parse(readFileSync(file, 'utf8'));
const slotInput = json(join(hd, '_site/slots.json'));
const review: Review[] = readFileSync(join(hd, 'review.jsonl'), 'utf8').trim().split('\n').filter(Boolean).map(line => JSON.parse(line));
const plan = imagePlan({ slots: slotInput, review, provenance: json(join(hd, '_site/provenance.json')), published: products.map(p => p.slug) });
const hash = (data: Uint8Array) => createHash('sha256').update(data).digest('hex');
const safeInput = (root: string, path: string) => {
  const input = realpathSync(resolve(root, path));
  if (!input.startsWith(realpathSync(root) + '/')) throw Error('Source escaped approved directory');
  return input;
};
const files: { url: string; bytes: number; sha256: string; sourceSha256: string }[] = [];
const dimensions = new Map<string, { w: number; h: number }>();
function record(url: string, sourceSha256: string) {
  const data = readFileSync(join(destination, url));
  files.push({ url, bytes: data.length, sha256: hash(data), sourceSha256 });
}
// Validate all review-bound originals before writing any deploy images.
for (const image of [...plan.slots, ...plan.packshots]) {
  const input = safeInput(hd, image.source), metadata = await sharp(input).metadata();
  if (hash(readFileSync(input)) !== image.sha256 || metadata.width !== image.w || metadata.height !== image.h) throw Error(`Reviewed source changed: ${image.id || image.slug}`);
}
for (const product of products) for (const image of [...product.images, ...product.size_chart.map(src => ({ src, web_sha256: '' }))]) {
  if (dimensions.has(image.src)) continue;
  const name = image.src.replace(/^\/img\//, '');
  if (!/^[a-z0-9-]+\/[a-z0-9-]+\.webp$/.test(name)) throw Error('Invalid published fallback path');
  const input = safeInput(join(sourceRoot, 'images'), name.replace(/^pullover-hoodie-/, 'aston-hoodie-'));
  const digest = hash(readFileSync(input));
  if (image.web_sha256 && digest !== image.web_sha256) throw Error('Fallback image changed');
  const output = join(destination, image.src), metadata = await sharp(input).metadata();
  if (!metadata.width || !metadata.height) throw Error('Missing fallback geometry');
  mkdirSync(dirname(output), { recursive: true }); copyFileSync(input, output);
  dimensions.set(image.src, { w: metadata.width, h: metadata.height }); record(image.src, digest);
}
for (const image of [...plan.slots, ...plan.packshots]) {
  const input = safeInput(hd, image.source);
  mkdirSync(dirname(join(destination, image.src)), { recursive: true });
  if (image.id) {
    for (const width of widths) for (const format of ['avif', 'webp']) {
      const url = `${imageBase(image.id)}-${width}.${format}`;
      await sharp(input).resize({ width })[format]({ quality: format === 'avif' ? 50 : 76 }).toFile(join(destination, url));
      record(url, image.sha256);
    }
  } else {
    await sharp(input).resize({ width: 1024 }).webp({ quality: 82 }).toFile(join(destination, image.src));
    record(image.src, image.sha256);
  }
  dimensions.set(image.src, { w: image.w, h: image.h });
}
const replacements = new Map(plan.packshots.map(image => [products.find(p => p.slug === image.slug)!.images[0].src, image.src]));
function htmlFiles(dir: string): string[] { return readdirSync(dir, { withFileTypes: true }).flatMap(entry => entry.isDirectory() ? htmlFiles(join(dir, entry.name)) : entry.name.endsWith('.html') ? [join(dir, entry.name)] : []); }
const html = htmlFiles(destination);
for (const file of html) {
  let text = readFileSync(file, 'utf8');
  for (const [fallback, approved] of replacements) text = text.replaceAll(`"${fallback}"`, `"${approved}"`);
  text = text.replace(/<img\b[^>]*>/g, tag => {
    const src = tag.match(/\bsrc="([^"]+)"/)?.[1], size = src && dimensions.get(src);
    if (!size) throw Error(`Unpackaged image: ${relative(destination, file)}`);
    return tag.replace(/\s(?:width|height)="[^"]*"/g, '').replace(/\s*\/?>$/, ` width="${size.w}" height="${size.h}">`);
  });
  if (/(?:src|href|srcset)="[^"\n]*\bhd\//.test(text)) throw Error('Private HD path in HTML');
  writeFileSync(file, text);
}
// The external inventory, not a copied build-time list, determines deploy completeness.
const targets: Record<string, string[]> = { home: [''], collection: ['collections', ...new Set(products.map(p => `collections/${p.category}`))], about: ['about'], oem: ['about'], pdp: products.map(p => `products/${p.slug}`), contact: ['contact'] };
for (const locale of ['en', 'es', 'fr', 'de', 'pt', 'ru', 'ar']) for (const image of plan.slots) {
  const page = image.id!.split('/')[0];
  if (!targets[page]) throw Error('Unknown page slot');
  for (const route of targets[page]) if (!readFileSync(join(destination, locale === 'en' ? '' : locale, route, 'index.html'), 'utf8').includes(`data-slot="${image.id}"`)) throw Error(`Unwired slot: ${image.id}`);
}
const receipt = (image: Approved) => ({ id: image.id, slug: image.slug, role: image.id ? 'illustrative' : image.role, src: image.src, sha256: image.sha256, w: image.w, h: image.h, rights: image.rights });
const manifest = { schema: 1, sha: execFileSync('git', ['rev-parse', 'HEAD'], { encoding: 'utf8' }).trim(), slots: plan.slots.map(receipt), packshots: plan.packshots.map(receipt), files };
writeFileSync(join(destination, 'deploy-manifest.json'), JSON.stringify(manifest, null, 2) + '\n');
console.log(JSON.stringify({ sha: manifest.sha, slots: plan.slots.length, packshots: plan.packshots.map(p => p.slug), files: files.length, hero1024: files.filter(p => p.url.includes('illustrative_home_hero-1024')) }));
