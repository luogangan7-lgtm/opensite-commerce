/** Shared G packager for deploy and local browser gates; exact reviewed bytes, square WebP <=1200, no upscale. */
import { readFileSync, realpathSync, mkdirSync } from 'node:fs';
import { resolve, dirname, join } from 'node:path';
import { createHash } from 'node:crypto';
import { createRequire } from 'node:module';
import { assertDetailDigest, type DetailSource } from './image-plan';
const sharp = createRequire(import.meta.resolve('astro/package.json'))('sharp');
export async function packageDetails(images: DetailSource[], sourceRoot: string, destination: string) {
  const root = realpathSync(sourceRoot);
  // Validate the complete batch before writing; read/hash the same bytes that sharp receives.
  const verified = await Promise.all(images.map(async image => {
    const input = realpathSync(resolve(root, image.source));
    if (!input.startsWith(root + '/')) throw Error('Detail source escaped approved directory');
    if (image.src !== `/img/${image.slug}/${image.role}.webp` || !/^[a-z0-9-]+$/.test(image.slug) || !['fabric','structure'].includes(image.role)) throw Error('Invalid detail output');
    const bytes = readFileSync(input);
    assertDetailDigest(image, createHash('sha256').update(bytes).digest('hex'));
    const meta = await sharp(bytes).metadata();
    if (!meta.width || meta.width !== meta.height) throw Error('Detail must be square');
    return { image, bytes };
  }));
  const receipts = [];
  for (const { image, bytes } of verified) {
    const output = join(destination, image.src);
    mkdirSync(dirname(output), { recursive: true });
    await sharp(bytes).resize({width:1200, height:1200, fit:'inside', withoutEnlargement:true}).webp({quality:82}).toFile(output);
    const meta = await sharp(output).metadata();
    receipts.push({src:image.src, sourceSha256:image.sha256, w:meta.width!, h:meta.height!});
  }
  return receipts;
}
