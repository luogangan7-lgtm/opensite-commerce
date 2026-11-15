/** CP3 gate: Home media masters are SHA-bound, delivered only as exact 1024/640 derivatives, never committed binaries. */
import { test, expect } from 'bun:test';
import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { digitalDelivery } from '../image-plan';
import media from '../home-media.json';
test('home-media masters are SHA-bound and unique with exact derivatives', () => {
 expect(media.map(row => row.id)).toEqual(['hero-visit', 'hero-overhead', 'hero-lifestyle', 'collections', 'oem', 'fabric', 'terms-one', 'terms-two']);
 expect(new Set(media.map(row => row.src)).size).toBe(media.length);
 for (const row of media) {
  expect(row.sha256).toMatch(/^[a-f0-9]{64}$/);
  expect(row.source.startsWith('/') || row.source.split('/').includes('..')).toBe(false);
  expect(digitalDelivery(row).map(file => file.width)).toEqual([1024, 640]);
 }
});
test('Home view reads copy from dictionaries and media only from the manifest', () => {
 const home = readFileSync(join(import.meta.dir, '../src/views/Home.astro'), 'utf8');
 expect(home).not.toMatch(/<Illustration|\.(png|jpe?g|webp)["']/);
 for (const id of ['collections', 'oem', 'fabric', 'terms-one', 'terms-two']) expect(home).toContain(`id="${id}"`);
});
test('no media binaries are committed under Home paths', () => {
 const root = join(import.meta.dir, '..');
 expect(existsSync(join(root, 'public/img/home'))).toBe(false);
 expect(readdirSync(root).some(name => /\.(png|jpe?g|webp|raw)$/.test(name))).toBe(false);
});
