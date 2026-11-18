/** CP3 gate: Home media masters are SHA-bound, delivered only as exact 1024/640 derivatives, never committed binaries. */
import { test, expect } from 'bun:test';
import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { digitalDelivery } from '../image-plan';
import media from '../home-media.json';
test('home-media masters are SHA-bound and unique with exact derivatives', () => {
 expect(media.map(row => row.id)).toEqual(['hero-visit', 'hero-overhead', 'hero-lifestyle', 'collections', 'oem', 'fabric', 'terms-one', 'terms-two', 'factory-floor']);
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
 for (const id of ['factory-floor', 'fabric', 'terms-one', 'terms-two']) expect(home).toContain(`id="${id}"`);
});
test('no media binaries are committed under Home paths', () => {
 const root = join(import.meta.dir, '..');
 expect(existsSync(join(root, 'public/img/home'))).toBe(false);
 expect(readdirSync(root).some(name => /\.(png|jpe?g|webp|raw)$/.test(name))).toBe(false);
});

test('fabric master matches the owner-picked Figma image at native 3:2 resolution', () => {
 const fabric = media.find(row => row.id === 'fabric')!;
 expect(fabric.width).toBeGreaterThanOrEqual(1024);
 expect(fabric.width / fabric.height).toBe(3 / 2);
 expect(fabric.sha256).toBe('fcd1a560e9783c1660adff9d21d879e9aaf0f9569ec40b05db5dbba107a11b7c');
 expect(fabric.authority).toContain('bd87eac7130384b0fdb2848d1afd2fbbe68e4a7b');
 expect(fabric.src).toBe('/img/home/fabric.webp');
});

// HOME-BM approved copy remains the literal rendered source, including reviewed locale deltas.
test('HOME-BM eight locale copy includes wholesale, private label and custom models', async () => {
 for (const locale of ['en','zh','es','fr','de','pt','ru','ar']) {
  const t=JSON.parse(readFileSync(join(import.meta.dir, `../src/i18n/${locale}.json`),'utf8'));
  for (const model of ['wholesale','private','custom']) for (const field of ['title','benefit','get','cta']) expect(t[`home.bm.${model}-${field}`].length).toBeGreaterThan(0);
  expect(t['home.bm.wholesale-title']).toStartWith('01 · ');
  expect(t['home.bm.private-title']).toStartWith('02 · ');
  expect(t['home.bm.custom-title']).toStartWith('03 · ');
 }
});
