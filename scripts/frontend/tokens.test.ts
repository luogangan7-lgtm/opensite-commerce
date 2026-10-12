// Token contract tests; consumes generator and contrast gate; checks deterministic derived files.
import { expect, test } from 'bun:test';
import { fileURLToPath } from 'node:url';
import { contrastRatio, checkContrast } from './check-contrast';
import source from '../../packages/design-tokens/tokens.json';
test('WCAG known extremes',()=>{expect(contrastRatio('#000000','#ffffff')).toBeCloseTo(21,8);expect(contrastRatio('#ffffff','#ffffff')).toBe(1);});
test('registry passes and broken foreground fails',()=>{expect(checkContrast(source)).toEqual([]);const bad=structuredClone(source);bad.tokens['color-muted']='#f7f8fa';expect(checkContrast(bad).length).toBeGreaterThan(0);});
test('malformed colors fail closed',()=>expect(()=>contrastRatio('white','#ffffff')).toThrow());
test('generated outputs are current',()=>{const p=Bun.spawnSync([process.execPath,'packages/design-tokens/generate.ts','--check'],{cwd: fileURLToPath(new URL('../../',import.meta.url))});expect(p.exitCode).toBe(0);});
test('every pair includes strict colors and thresholds; placeholders covered by muted',()=>{expect(source.contrastPairs.filter(p=>p.foreground==='color-muted').length).toBe(2);for(const pair of source.contrastPairs) expect(contrastRatio(source.tokens[pair.foreground as keyof typeof source.tokens],source.tokens[pair.background as keyof typeof source.tokens])).toBeGreaterThanOrEqual(pair.minimum);});
import { renderTokens } from '../../packages/design-tokens/generate';
test('single source renders deterministic outputs and token-safe typed accessor',()=>{const rendered=renderTokens(source.tokens);expect(rendered).toEqual(renderTokens(Object.fromEntries(Object.entries(source.tokens).reverse())));for(const [name,value] of Object.entries(source.tokens)) expect(rendered.css).toContain(`--${name}: ${value};`);expect(rendered.ts).toContain('keyof typeof tokens');});
test('invalid token names and CSS injection fail closed',()=>{expect(()=>renderTokens({'bad key':'1rem'})).toThrow();expect(()=>renderTokens({'space-1':'1rem; color:red'})).toThrow();});
