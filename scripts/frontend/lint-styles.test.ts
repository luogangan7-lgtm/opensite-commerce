// Style gate regression tests; consumes lintText; fails on literals and physical spacing.
import { expect, test } from 'bun:test';
import { lintText } from './lint-styles';
for (const source of ['a { color: #fff; }','a { color: rgb(1 2 3); }','a { padding: 12px; }','a { gap: 1rem; }','a { font-size: 100%; }','a { margin-left: var(--space-1); }','const s = {fontSize: 14, paddingInline: 4};','const s = {color: "red"};']) {
  test(`reject ${source}`, () => expect(lintText(source,'probe.tsx').length).toBeGreaterThan(0));
}
test('accept token-derived CSS and inline styles',()=>expect(lintText('a { padding-inline: var(--space-2); gap: calc(var(--space-1) * 2); font-size: var(--font-size-md); margin: 0; } const s = {color: "var(--color-text)"};','probe.tsx')).toEqual([]));
test('token presence cannot hide a literal in calc',()=>expect(lintText('a { gap: calc(var(--space-2) + 3px); }','probe.css').length).toBeGreaterThan(0));
test('literal inline font size and logical spacing are rejected individually',()=>{for(const text of ['const s = {fontSize: 14};','const s = {paddingInline: 4};','const s = {borderRadius: "50%"};']) expect(lintText(text,'probe.tsx').length).toBeGreaterThan(0);});
import { collectStyleFiles } from './lint-styles';
import { mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
test('generated bundles and dependencies are excluded while source is scanned',()=>{
 const directory=fileURLToPath(new URL('../../work/c08-tokens/lint-probe',import.meta.url));
 try {
  for(const name of ['src','dist','dist-gallery','node_modules','build']) {mkdirSync(`${directory}/${name}`,{recursive:true});writeFileSync(`${directory}/${name}/style.css`,'a {color:#fff}');}
  expect(collectStyleFiles(directory)).toEqual([`${directory}/src/style.css`]);
 } finally {rmSync(directory,{recursive:true,force:true});}
});
