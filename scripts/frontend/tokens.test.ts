// Token contract tests; consumes generator and contrast gate; checks deterministic derived files.
import { expect, test } from 'bun:test';
import { fileURLToPath } from 'node:url';
import { contrastRatio, checkContrast } from './check-contrast';
import source from '../../packages/design-tokens/tokens.json';
test('WCAG known extremes',()=>{expect(contrastRatio('#000000','#ffffff')).toBeCloseTo(21,8);expect(contrastRatio('#ffffff','#ffffff')).toBe(1);});
test('registry passes and broken foreground fails',()=>{expect(checkContrast(source)).toEqual([]);const bad=structuredClone(source);bad.tokens['color-muted']='#f7f8fa';expect(checkContrast(bad).length).toBeGreaterThan(0);});
// These pairs occur in danger hover, FormField errors, links/ghost buttons and disabled Checkbox labels.
test('actual component text pairs are registered at WCAG AA text thresholds',()=>{
  const textPairs = [
    ['color-on-brand','color-danger'],
    ['color-danger','color-surface'], ['color-danger','color-canvas'],
    ['color-brand','color-surface'], ['color-brand','color-canvas'],
    ['color-disabled','color-surface'], ['color-disabled','color-canvas'],
  ];
  for (const [foreground,background] of textPairs) {
    const pair = source.contrastPairs.find(pair=>pair.foreground===foreground && pair.background===background);
    expect(pair, `${foreground} on ${background}`).toBeDefined();
    expect(pair!.minimum).toBeGreaterThanOrEqual(4.5);
  }
});
test('brand text on canvas rejects a token that only met the old non-text threshold',()=>{
  const bad=structuredClone(source);
  bad.tokens['color-brand']='#747474';
  expect(contrastRatio(bad.tokens['color-brand'],bad.tokens['color-surface'])).toBeGreaterThanOrEqual(4.5);
  expect(contrastRatio(bad.tokens['color-brand'],bad.tokens['color-canvas'])).toBeGreaterThanOrEqual(3);
  expect(contrastRatio(bad.tokens['color-brand'],bad.tokens['color-canvas'])).toBeLessThan(4.5);
  expect(checkContrast(bad).some(error=>error.startsWith('brand text on canvas:'))).toBe(true);
});
test('danger hover rejects light danger even when its semantic badge pair passes',()=>{
  const bad=structuredClone(source);
  bad.tokens['color-danger']='#fca5a5';
  bad.tokens['color-danger-bg']='#450a0a';
  expect(contrastRatio(bad.tokens['color-danger'],bad.tokens['color-danger-bg'])).toBeGreaterThanOrEqual(4.5);
  expect(checkContrast(bad).some(error=>error.startsWith('on-brand on danger:'))).toBe(true);
});
test('malformed colors fail closed',()=>expect(()=>contrastRatio('white','#ffffff')).toThrow());
test('generated outputs are current',()=>{const p=Bun.spawnSync([process.execPath,'packages/design-tokens/generate.ts','--check'],{cwd: fileURLToPath(new URL('../../',import.meta.url))});expect(p.exitCode).toBe(0);});
test('every pair includes strict colors and thresholds; placeholders covered by muted',()=>{expect(source.contrastPairs.filter(p=>p.foreground==='color-muted').length).toBe(2);for(const pair of source.contrastPairs) expect(contrastRatio(source.tokens[pair.foreground as keyof typeof source.tokens],source.tokens[pair.background as keyof typeof source.tokens])).toBeGreaterThanOrEqual(pair.minimum);});
import { renderTokens } from '../../packages/design-tokens/generate';
test('single source renders deterministic outputs and token-safe typed accessor',()=>{const rendered=renderTokens(source.tokens);expect(rendered).toEqual(renderTokens(Object.fromEntries(Object.entries(source.tokens).reverse())));for(const [name,value] of Object.entries(source.tokens)) expect(rendered.css).toContain(`--${name}: ${value};`);expect(rendered.ts).toContain('keyof typeof tokens');});
test('invalid token names and CSS injection fail closed',()=>{expect(()=>renderTokens({'bad key':'1rem'})).toThrow();expect(()=>renderTokens({'space-1':'1rem; color:red'})).toThrow();});
