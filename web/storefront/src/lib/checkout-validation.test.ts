/** Responsibility: checkout boundary gate; dependencies: Bun/public validation; caller: focused frontend gate. */
import { expect, test } from 'bun:test';
import { validateCheckout, checkoutInput, type CheckoutFields } from './checkout-validation';
const valid: CheckoutFields = { email: 'buyer@example.test', name: 'Ada', phone: '', country: 'US', address1: '1 Main Street', address2: '', city: 'Boston', subdivision: 'MA', postal_code: '02108' };
test('address and guest contact require complete bounded backend fields', () => {
  expect(validateCheckout(valid)).toEqual({});
  for (const field of ['email', 'country', 'address1', 'city', 'postal_code'] as const) {
    expect(validateCheckout({ ...valid, [field]: ' ' })[field]).toBeDefined();
  }
  for (const country of ['us', 'USA', 'U1', ' US', 'US ']) expect(validateCheckout({ ...valid, country }).country).toBe('country');
  for (const [field, length] of [['address1', 257], ['address2', 257], ['city', 129], ['subdivision', 129], ['postal_code', 33], ['name', 257], ['phone', 65], ['email', 255]] as const) {
    expect(validateCheckout({ ...valid, [field]: 'x'.repeat(length) })[field]).toBeDefined();
  }
  for (const email of ['buyer', 'a@', '@a.test', 'a b@example.test', 'a@example']) expect(validateCheckout({ ...valid, email }).email).toBe('email');
});
test('payload preserves country and normalizes contact without emitting empty optional fields', () => {
  expect(checkoutInput({ ...valid, address1: ' 1 Main Street ', name: ' Ada ', email: ' buyer@example.test ' })).toEqual({
    shipping_address: { country: 'US', address1: '1 Main Street', city: 'Boston', subdivision: 'MA', postal_code: '02108' },
    buyer_contact: { email: 'buyer@example.test', name: 'Ada' },
  });
});
test('customer checkout validates only address and never emits guest buyer_contact', () => {
  const fields = { ...valid, email: '', name: '', phone: '' };
  expect(validateCheckout(fields, 'customer')).toEqual({});
  expect(checkoutInput(fields, 'customer')).toEqual({ shipping_address: { country: 'US', address1: '1 Main Street', city: 'Boston', subdivision: 'MA', postal_code: '02108' } });
  expect(validateCheckout({ ...fields, country: 'us' }, 'customer').country).toBe('country');
});
test('name and subdivision follow checkout contract limits', () => {
  expect(validateCheckout({ ...valid, name: 'n'.repeat(200), subdivision: 's'.repeat(100) })).toEqual({});
  expect(validateCheckout({ ...valid, name: 'n'.repeat(201) }).name).toBe('tooLong');
  expect(validateCheckout({ ...valid, subdivision: 's'.repeat(101) }).subdivision).toBe('tooLong');
});
