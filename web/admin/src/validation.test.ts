/** Input trust-boundary gate; uses public validation helpers and native File only. */
import { expect, test } from 'bun:test';
import { validateCode, validateMediaFile, validateUUID, parseOptionValueIds, formatMinorMoney } from './validation';
test('ASCII code grammar rejects Unicode, paths, controls and empty values', () => {
  expect(validateCode(' item_1-OK ')).toBeUndefined();
  for (const invalid of ['', '   ', 'a/b', 'é', 'A B', 'a\nb', 'a'.repeat(129)]) expect(validateCode(invalid)).toBeString();
});
test('option UUID parser rejects duplicates and invalid input while preserving ordering', () => {
  const id = '11111111-1111-4111-8111-111111111111'; expect(validateUUID(id)).toBe(true);
  expect(parseOptionValueIds('')).toEqual([]); expect(parseOptionValueIds(id)).toEqual([id]);
  expect(() => parseOptionValueIds(`${id},${id}`)).toThrow(); expect(() => parseOptionValueIds('not-id')).toThrow();
});
test('upload client preflight rejects oversized, SVG and zero byte files', () => {
  expect(validateMediaFile(new File(['x'], 'good.png', { type: 'image/png' }))).toBeUndefined();
  for (const f of [new File([], 'empty.png', { type: 'image/png' }), new File(['x'], 'x.svg', { type: 'image/svg+xml' }), new File([new Uint8Array(10 * 1024 * 1024 + 1)], 'big.png', { type: 'image/png' })]) expect(validateMediaFile(f)).toBeString();
});
test('money minor formatting keeps arbitrary integer precision, rejects unsafe numeric inputs', () => {
  expect(formatMinorMoney('9007199254740993', 2)).toBe('90071992547409.93');
  expect(formatMinorMoney('-1', 2)).toBe('-0.01'); expect(formatMinorMoney(100, 0)).toBe('100');
  expect(() => formatMinorMoney(Number.MAX_SAFE_INTEGER + 1, 2)).toThrow();
});
