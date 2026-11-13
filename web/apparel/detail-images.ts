/** Shared G packager for deploy and local browser gates; exact reviewed bytes, native 1200-square only, no resizing can repair an undersized source. */
import { readFileSync, realpathSync, mkdirSync } from 'node:fs';
import { resolve, dirname, join } from 'node:path';
import { createHash } from 'node:crypto';
import { createRequire } from 'node:module';
import { assertDetailDigest, type DetailSource, type DetailLedger } from './image-plan';
const sharp = createRequire(import.meta.resolve('astro/package.json'))('sharp');
export const detailLedgerPath = 'fleet-20261104/launch-orchestration-20261112/project-control-02/g-connect-prep-01/mapping.json';
export const readDetailLedger = (sourceRoot: string): DetailLedger[] => JSON.parse(readFileSync(join(sourceRoot, detailLedgerPath), 'utf8'));
export async function packageDetails(images: DetailSource[], sourceRoot: string, destination: string, ledger = readDetailLedger(sourceRoot)) {
  const root = realpathSync(sourceRoot);
  // Validate the complete batch before writing; read/hash the same bytes that sharp receives.
  const verified = await Promise.all(images.map(async image => {
    const input = realpathSync(resolve(root, image.source));
    if (!input.startsWith(root + '/')) throw Error('Detail source escaped approved directory');
    if (image.src !== `/img/${image.slug}/${image.role}.webp` || !/^[a-z0-9-]+$/.test(image.slug) || !['fabric','structure'].includes(image.role)) throw Error('Invalid detail output');
    const bytes = readFileSync(input);
    assertDetailDigest(image, createHash('sha256').update(bytes).digest('hex'), ledger);
    const meta = await sharp(bytes).metadata();
    if (meta.width !== 1200 || meta.height !== 1200) throw Error('Detail requires native 1200×1200 pixels');
    return { image, bytes };
  }));
  const receipts = [];
  for (const { image, bytes } of verified) {
    const output = join(destination, image.src);
    mkdirSync(dirname(output), { recursive: true });
    await sharp(bytes).webp({quality:82}).toFile(output);
    const meta = await sharp(output).metadata();
    receipts.push({src:image.src, sourceSha256:image.sha256, w:meta.width!, h:meta.height!});
  }
  return receipts;
}
