/** CI source policy: only frozen catalog derivatives and pinned brand identity assets
 * may be committed. Deployment media belong in master-SHA manifests, not Git.
 * Called before the apparel build and by its unit suite; no private sources needed. */
import { test, expect } from 'bun:test';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import frozen from '../design/collection-frozen.json';

const root = execFileSync('git', ['rev-parse', '--show-toplevel'], { encoding: 'utf8' }).trim();
const allowed = new Map<string, string>([
 ...[...frozen.assets, ...frozen.catalog].flatMap(image => image.derivatives.map(file => [`web/apparel/public${file.src}`, file.sha256] as const)),
 // Existing brand identity is the only raster/vector exception outside frozen catalog delivery.
 ['web/apparel/public/brand/icon-16.png', '4acd81ed096498607574488456d7564d90ad5e078df976aa8db7081dca42f5f6'],
 ['web/apparel/public/brand/icon-32.png', '23394b0b7ab937e83b1a31ef45104f5db5decffb6eb93bf871b60d048b270872'],
 ['web/apparel/public/brand/icon-180.png', '155da17ad7ddd4f17bcdb5975a7ab5d71518c4ebe4e428c0ee8f9c6d09dbc470'],
 ['web/apparel/public/brand/primary.svg', '2cea43beb0d84a9286745f2a2b9759f90594d6e1b1294aa19ef1a5825d347051'],
 ['web/apparel/public/brand/reverse.svg', '5c6f26427355b6f3833a2b2081f852074ffa6ce0be2a942c8d1e1e3bae19cece'],
 ['web/apparel/public/brand/mono-ink.svg', '3788b686dd9529b0aeabccee6f252f737c8ab1c2369c7645a969660ead611df2'],
]);
function imageFile(path: string, bytes: Uint8Array) {
 const b = Buffer.from(bytes), text = b.subarray(0, 1024).toString('utf8').trimStart();
 return /\.(?:png|jpe?g|webp|avif|gif|ico|bmp|tiff?|heic|heif|jxl|svg)$/i.test(path)
  || b.subarray(0, 8).equals(Buffer.from([137,80,78,71,13,10,26,10]))
  || b.subarray(0, 3).equals(Buffer.from([255,216,255]))
  || /^GIF8[79]a/.test(b.subarray(0, 6).toString('ascii'))
  || b.subarray(0, 2).toString('ascii') === 'BM'
  || b.subarray(0, 4).equals(Buffer.from([0,0,1,0]))
  || b.subarray(0, 4).equals(Buffer.from([73,73,42,0])) || b.subarray(0, 4).equals(Buffer.from([77,77,0,42]))
  || b.subarray(0, 4).toString('ascii') === 'RIFF' && b.subarray(8, 12).toString('ascii') === 'WEBP'
  || b.subarray(4, 8).toString('ascii') === 'ftyp' && /avif|avis|heic|heix|mif1/.test(b.subarray(8, 32).toString('ascii'))
  || b.subarray(0, 2).equals(Buffer.from([255,10])) || b.subarray(4, 8).toString('ascii') === 'JXL '
  || /^(?:<svg\b|<\?xml\b[\s\S]*?<svg\b)/i.test(text);
}
function violations(files: { path: string; bytes: Uint8Array }[]) {
 return files.filter(({path,bytes}) => (imageFile(path,bytes) || allowed.has(path))
  && allowed.get(path) !== createHash('sha256').update(bytes).digest('hex')).map(file => file.path);
}
test('committed apparel images are limited to exact frozen catalog and brand SHA allow-list', () => {
 const paths = execFileSync('git', ['ls-files', '-z', '--', 'web/apparel'], { cwd: root, encoding: 'utf8' }).split('\0').filter(Boolean);
 const rejected = violations(paths.map(path => ({path,bytes:readFileSync(join(root,path))})));
 expect(rejected, 'Unapproved committed images; use SHA-bound media manifests and deploy packaging').toEqual([]);
});
test('media gate rejects new public images and images disguised outside public', () => {
 const png = Buffer.from([137,80,78,71,13,10,26,10]);
 const paths = ['web/apparel/public/img/about/new.webp', 'web/apparel/src/disguised.bin', 'web/apparel/dist/disguised.txt'];
 expect(violations(paths.map(path => ({path,bytes:png})))).toEqual(paths);
});
test('media allow-list rejects changed approved bytes', () => {
 const path = 'web/apparel/public/brand/icon-16.png';
 expect(violations([{path,bytes:Buffer.from('changed')}])).toEqual([path]);
});
