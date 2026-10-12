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

for (const source of [
  'a { background: red; }',
  'a { border: var(--border-width) solid red; }',
  'a { background: white; }',
  'a { outline: var(--focus-width) solid rebeccapurple; }',
  'a { box-shadow: var(--shadow-offset) var(--shadow-offset) var(--shadow-blur) papayawhip; }',
  'a { box-shadow: var(--shadow-sm), var(--shadow-offset) var(--shadow-offset) red; }',
  'a { background: var(--color-surface) red; }',
  'a { background: linear-gradient(var(--color-brand), red); }',
  'a { color: var(--color-text, red); }',
  'a { background: var(--color-surface, var(--color-brand, white)); }',
  'a { fill: gold; stroke: steelblue; }',
  'a { border-block-end: var(--border-width) solid aliceblue; }',
  'a { color: Red; background: WHite; }',
  'const style = { background: "red", border: "var(--border-width) solid white" };',
  'const style = { "background": "red" };',
  "const style = { 'outline': 'var(--focus-width) solid red' };",
  'const style = { boxShadow: "var(--shadow-sm), var(--shadow-offset) var(--shadow-offset) red" };',
  'const style = { backgroundColor: "var(--color-surface, red)" };',
  'const style = { backgroundImage: "linear-gradient(var(--color-brand), red)" };',
  'const style = { color: "var(--color-text)", background: "red" };',
  'a:hover { background: red; }',
  'a:is(:hover, :focus-visible) { outline: var(--focus-width) solid red; }',
]) {
  test(`reject declaration color ${source}`, () => expect(lintText(source, 'probe.tsx').length).toBeGreaterThan(0));
}
for (const source of [
  'a { background: var(--color-surface); border: var(--border-width) solid var(--color-border); }',
  'a { outline: var(--focus-width) solid currentColor; background: transparent; color: inherit; }',
  'a { background: transparent !important; color: currentColor !important; box-shadow: none !important; }',
  'a { box-shadow: var(--shadow-sm), var(--shadow-lg); }',
  'a { background: linear-gradient(to right, var(--color-surface), var(--color-brand)); }',
  'a { background: color-mix(in srgb, var(--color-text) 55%, transparent); }',
  'a { background: var(--color-surface, var(--color-brand)); }',
  'a { border-block-end: var(--border-width) solid var(--color-border); fill: currentColor; stroke: none; }',
  'const style = { "background": "var(--color-surface)", boxShadow: "var(--shadow-sm)", color: "var(--color-text)" };',
  'const label = { text: "background: red" };',
  'const caption = "background: red";',
  "const caption = 'outline: var(--focus-width) solid red';",
  '/* background: red */ a { background: var(--color-surface); }',
  'const style = { backgroundImage: "linear-gradient(var(--color-surface), var(--color-brand))" };',
  'const style = { background: "var(--color-surface)", margin: 0, };',
]) {
  test(`accept declaration token color ${source}`, () => expect(lintText(source, 'probe.tsx')).toEqual([]));
}
test('quoted layout properties retain the token-only rule', () => {
  expect(lintText('const style = { "fontSize": 14, "paddingInline": 4 };', 'probe.tsx').length).toBeGreaterThan(0);
  expect(lintText('const style = { "paddingInline": "var(--space-2)", "fontSize": "var(--font-size-md)" };', 'probe.tsx')).toEqual([]);
});
test('quoted physical spacing cannot bypass the logical direction rule', () => {
  for (const source of ['const style = { "marginLeft": "var(--space-2)" };', "const style = { 'padding-right': 'var(--space-2)' };"]) {
    expect(lintText(source, 'probe.tsx').length).toBeGreaterThan(0);
  }
});
