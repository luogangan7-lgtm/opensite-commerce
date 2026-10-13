/** Tests the actual style scanner boundary, called by the source gate; Astro templates are first-party token consumers. */
import { expect, test } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { collectStyleFiles, lintText } from './lint-styles';
test('Astro templates are scanned and literal styles fail', () => {
  mkdirSync('.work', {recursive:true});
  const directory=mkdtempSync('.work/storefront-style-');
  try {
    const path=join(directory,'page.astro');
    writeFileSync(path,'<style>.page { background: red; padding: 12px; }</style>');
    expect(collectStyleFiles(directory)).toContain(path);
    expect(lintText('<style>.page { background: red; padding: 12px; }</style>',path).length).toBeGreaterThan(0);
    expect(lintText('<style>.page { background: var(--color-canvas); padding: var(--space-4); }</style>',path)).toEqual([]);
  } finally {rmSync(directory,{recursive:true,force:true});}
});
