/** Purchasing transport regressions; browser-safe DTOs and exact integer handling. */
import { describe, expect, test } from 'bun:test';
import { parseIntegerJSON, safeInteger, minorAmount } from './commerce-wire';
describe('purchasing wire integers', () => {
  test('preserves signed int64 lexemes without touching strings', () => {
    expect(parseIntegerJSON('{"amount":9223372036854775807,"label":"123 \\\"456\\\"","quantity":2}')).toEqual({ amount: '9223372036854775807', label: '123 "456"', quantity: '2' });
  });
  test('rejects fractional/exponent amounts, overflow and invalid quantities', () => {
    for (const value of ['1.5','1e3','-1','9223372036854775808','01']) expect(() => minorAmount(value)).toThrow();
    expect(minorAmount('9223372036854775807')).toBe('9223372036854775807');
    expect(safeInteger('1000000', 0, 1000000)).toBe(1000000);
    for (const value of ['1000001','9007199254740993','1.5','-1']) expect(() => safeInteger(value,0,1000000)).toThrow();
  });
  test('rejects oversized and malformed upstream JSON', () => {
    expect(() => parseIntegerJSON('x'.repeat(1048577))).toThrow();
    expect(() => parseIntegerJSON('{"n":01}')).toThrow();
  });
});
