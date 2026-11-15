/** Figma snapshot is the independent oracle; apparel CI runs this with the unit suite.
 * Reads generated CSS and responsive apparel rules. No live service or writes. */
import { test, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import snapshot from '../design/figma-variables.json';
const css = readFileSync(new URL('../../../packages/design-tokens/src/tokens.css', import.meta.url), 'utf8');
const styles = readFileSync(new URL('../src/styles.css', import.meta.url), 'utf8');
const variables = snapshot.variables;
function value(v: typeof variables[number], mode: string, seen = new Set<string>()): string {
  if (seen.has(v.id)) throw Error(`Cyclic alias ${v.name}`);
  seen.add(v.id);
  const collection = snapshot.collections.find(c => c.id === v.collection)!;
  const raw: any = v.values[mode as keyof typeof v.values] ?? v.values[collection.defaultModeId as keyof typeof v.values];
  if (raw?.type === 'VARIABLE_ALIAS') return value(variables.find(x => x.id === raw.id)!, mode, seen);
  if (v.type === 'COLOR') return '#' + [raw.r,raw.g,raw.b].map(n => Math.round(n*255).toString(16).padStart(2,'0')).join('') + (raw.a === 1 ? '' : Math.round(raw.a*255).toString(16).padStart(2,'0'));
  return `${raw}px`;
}
function normalized(s?: string) {
  if (!s) return s;
  if (/^[.\d]+rem$/.test(s)) return `${parseFloat(s)*16}px`;
  if (/^0(?:px)?$/.test(s)) return '0px';
  return s.toLowerCase();
}
const declarations = (text: string) => new Map([...text.matchAll(/--([\w-]+):\s*([^;]+);/g)].map(m => [m[1],m[2].trim()]));
const tokens = declarations(css);
for (const v of variables) {
  const collection = snapshot.collections.find(c => c.id === v.collection)!;
  const name = v.name.startsWith('sy-') ? v.name : `sy-primitive-${v.name.replaceAll('/','-')}`;
  for (const mode of collection.modes) test(`${v.name} / ${mode.name}`, () => {
    const mobile = mode.name === 'Mobile';
    expect(normalized(tokens.get(name + (mobile ? '-mobile' : '')))).toBe(value(v,mode.modeId));
    if (mobile) {
      const block = styles.match(/@media \(max-width: 650px\)\s*\{\s*:root\s*\{([^}]+)\}/)?.[1] || '';
      expect(declarations(block).get(name)).toBe(`var(--${name}-mobile)`);
    }
  });
}
