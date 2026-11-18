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
  return v.name.startsWith('sy-weight-') ? String(raw) : `${raw}px`;
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
      const block = styles.match(/@media \(max-width: 650px\)\s*\{\s*body\s*\{([^}]+)\}/)?.[1] || '';
      expect(declarations(block).get(name)).toBe(`var(--${name}-mobile)`);
    }
  });
}

// Font families and weights come from current native text/style reads; numeric/color
// variables alone cannot detect a font substitution (the CP0 regression).
const typography = snapshot.typography;
for (const [role, expected] of Object.entries({
  'font-body': typography.latin.body, 'font-heading': typography.latin.heading,
  'weight-heading': typography.latin.headingWeight, 'weight-title': typography.latin.titleWeight,
  'weight-body': typography.latin.bodyWeight, 'weight-label': typography.latin.labelWeight,
  'font-body-ar': typography.ar.body, 'font-heading-ar': typography.ar.heading,
  'weight-heading-ar': typography.ar.headingWeight,
  'font-body-zh': typography.zh.body, 'font-heading-zh': typography.zh.heading,
})) test(`frozen typography ${role}`, () => {
  const actual = tokens.get(`sy-${role}`);
  if (typeof expected === 'number') expect(actual).toBe(String(expected));
  else expect(actual?.split(',')[0].trim().replaceAll('"','')).toBe(expected);
});
for (const locale of ['ar','zh']) test(`locale ${locale} applies its own font tokens`, () => {
 const block = styles.match(new RegExp(`:root:lang\\(${locale}\\)\\s*\\{([^}]+)\\}`))?.[1] || '';
 const values = declarations(block);
 expect(values.get('sy-font-body')).toBe(`var(--sy-font-body-${locale})`);
 expect(values.get('sy-font-heading')).toBe(`var(--sy-font-heading-${locale})`);
 if (locale === 'ar') expect(values.get('sy-weight-heading')).toBe('var(--sy-weight-heading-ar)');
});

// Card elevation is exported from the native effect style, independently of CSS.
test('card elevation matches the native Figma effect', () => {
 const e = snapshot.elevation.find(s => s.name === 'Sourcyo/Elevation/Card')!.effects[0];
 const c = e.color;
 const expected = `${e.offset.x}px ${e.offset.y}px ${e.radius}px ${e.spread}px rgb(${Math.round(c.r*100)}% ${Math.round(c.g*100)}% ${Math.round(c.b*100)}% / ${Math.round(c.a*100)}%)`;
 expect(tokens.get('sy-shadow-card')).toBe(expected);
});
