/** Checks the generated returns wire contract; called by tsc/Bun; money stays exact in JavaScript. */
import { expect, test } from 'bun:test';
import type { components } from '../../packages/api-client/src/schema';

test('return refund minor units use the lossless decimal string contract', () => {
  const amount: components['schemas']['ReturnRecord']['refund_minor'] = '9007199254740993';
  // @ts-expect-error Financial minor units must not accept lossy JavaScript numbers.
  const invalidAmount: components['schemas']['ReturnRecord']['refund_minor'] = 9007199254740993;
  void invalidAmount;
  expect(JSON.parse(JSON.stringify({ refund_minor: amount })).refund_minor).toBe('9007199254740993');
  expect(BigInt(amount)).toBe(9007199254740993n);
});
