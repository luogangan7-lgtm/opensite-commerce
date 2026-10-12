/** Responsibility: storefront boundary regressions; callers: Bun frontend gate. */
import { describe, expect, test } from 'bun:test';
import { formatAmount, normalizeHostname, readAPIOrigin, safeHref } from './presentation';

describe('storefront server and presentation boundaries', () => {
  test('only explicit HTTP origins without credentials or request-derived paths', () => {
    expect(readAPIOrigin('http://127.0.0.1:4175')).toBe('http://127.0.0.1:4175');
    expect(readAPIOrigin('https://read.example.test/')).toBe('https://read.example.test');
    for (const value of ['file:///etc/passwd', 'https://user:secret@example.test', 'https://api.test/path', 'https://api.test/?host=other', 'https://api.test/#fragment', '']) {
      expect(() => readAPIOrigin(value)).toThrow();
    }
  });
  test('host normalization is independent of forwarded headers', () => {
    expect(normalizeHostname('SHOP.Example.test.')).toBe('shop.example.test');
    expect(normalizeHostname('127.0.0.1')).toBe('127.0.0.1');
    for (const host of ['host:123', 'bad host', '', 'a/b', 'a@b']) expect(() => normalizeHostname(host)).toThrow();
  });
  test('published navigation cannot turn into executable or off-site links', () => {
    expect(safeHref('/about')).toBe('/about');
    expect(safeHref('/products/widget')).toBe('/products/widget');
    for (const href of ['//other.test', 'javascript:alert(1)', '/\\other.test', '/\nother', 'https://other.test', '/%2f%2fevil.test']) expect(safeHref(href)).toBeNull();
  });
  test('minor-unit strings retain precision and currency exponents', () => {
    expect(formatAmount('1299', 'USD', 'en-US')).toBe('$12.99');
    expect(formatAmount('1234', 'JPY', 'en-US')).toBe('¥1,234');
    expect(formatAmount('1234', 'KWD', 'en-US')).toBe('KWD 1.234');
    expect(formatAmount('900719925474099312', 'USD', 'en-US')).toBe('$9,007,199,254,740,993.12');
    for (const amount of ['12.5', '-1', 'Infinity', '1e4']) expect(() => formatAmount(amount, 'USD', 'en-US')).toThrow();
  });
});
