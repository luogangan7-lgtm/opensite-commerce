/** Deploy packaging only; copies exact catalog-approved files, never builds or publishes other slugs. */
import { copyFileSync, mkdirSync, readFileSync } from 'node:fs';
import { dirname, resolve, basename } from 'node:path';
import { execFileSync } from 'node:child_process';
import products from './src/data/products.json';
const common = execFileSync('git', ['rev-parse', '--git-common-dir'], { encoding: 'utf8' }).trim();
const source = process.env.APPAREL_IMAGES_DIR || resolve(dirname(resolve(common)), 'work/apparel-b2b/images');
const destination = process.argv[2];
if (!destination) throw Error('Pass an empty downloaded apparel-dist directory');
for (const product of products) {
  for (const image of [...product.images, ...product.size_chart.map(src => ({ src, web_sha256: '' }))]) {
    const relative = image.src.replace(/^\/img\//, '');
    if (!/^[a-z0-9-]+\/[a-z0-9-]+\.webp$/.test(relative)) throw Error('Invalid published image path');
    const original = relative.replace(/^pullover-hoodie-/, 'aston-hoodie-');
    const input = resolve(source, original); const output = resolve(destination, 'img', relative);
    if (image.web_sha256 && new Bun.CryptoHasher('sha256').update(readFileSync(input)).digest('hex') !== image.web_sha256) throw Error(`Image changed: ${basename(relative)}`);
    mkdirSync(dirname(output), { recursive: true }); copyFileSync(input, output);
  }
}
console.log('Packaged published product images and approved referenced charts.');
