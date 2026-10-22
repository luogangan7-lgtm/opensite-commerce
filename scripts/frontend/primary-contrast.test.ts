/** Primary-button contrast contract; reads shared CSS/tokens, run by frontend CI.
 * No animated color/opacity may introduce a frame below AA, including pending→idle. */
import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import source from '../../packages/design-tokens/tokens.json';
import { contrastRatio } from './check-contrast';

const css = readFileSync(new URL('../../packages/ui/styles.css', import.meta.url), 'utf8');
function declarations(selector: string) {
  const rule = [...css.matchAll(/([^{}]+)\{([^{}]*)\}/g)]
    .find(match => match[1].trim() === selector);
  expect(rule, selector).toBeDefined();
  return Object.fromEntries(rule![2].split(';').filter(part => part.includes(':'))
    .map(part => { const at = part.indexOf(':'); return [part.slice(0, at).trim(), part.slice(at + 1).trim()]; }));
}
function token(value: string) {
  const match = /^var\(--([\w-]+)\)$/.exec(value);
  expect(match, `opaque token required: ${value}`).not.toBeNull();
  return source.tokens[match![1] as keyof typeof source.tokens];
}

test('primary states use opaque tokens with white text at AA', () => {
  const idle = declarations('.os-button--primary');
  const hover = { ...idle, ...declarations('.os-button--primary:hover:not(:disabled)') };
  const disabled = { ...idle, ...declarations('.os-button--primary:is(:disabled, [aria-disabled="true"], [aria-busy="true"])') };
  for (const [name, state] of Object.entries({ idle, hover, focus: idle, active: hover,
    pending: disabled, submitting: disabled, disabled, focusableDisabled: disabled })) {
    expect(token(state.color), name).toBe('#ffffff');
    expect(contrastRatio(token(state.color), token(state.background)), name).toBeGreaterThanOrEqual(4.5);
  }
});
test('primary cannot animate a low-contrast intermediate frame', () => {
  expect(declarations('.os-button--primary').transition).toBe('none');
});
test('the observed intermediate color is rejected, not rounded up', () => {
  expect(contrastRatio('#ffffff', '#6579b0')).toBeCloseTo(4.27, 2);
  expect(contrastRatio('#ffffff', '#6579b0')).toBeLessThan(4.5);
});
