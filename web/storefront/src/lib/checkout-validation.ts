/** Responsibility: PR38-aligned address/contact boundary validation.
 * Dependencies: purchase contract; caller: checkout form. Invariants: bounded plain fields, uppercase ISO-style country, no money calculations.
 */
import type { CheckoutInput, PurchaseContext } from './commerce-contract';
export type CheckoutFields = { email: string; name: string; phone: string; country: string; address1: string; address2: string; city: string; subdivision: string; postal_code: string };
export type ValidationError = 'required' | 'tooLong' | 'country' | 'email';
export type CheckoutErrors = Partial<Record<keyof CheckoutFields, ValidationError>>;
export function validateCheckout(fields: CheckoutFields, actor: PurchaseContext['actor'] = 'guest'): CheckoutErrors {
  const errors: CheckoutErrors = {};
  const limits = { email: 254, name: 200, phone: 64, country: 2, address1: 256, address2: 256, city: 128, subdivision: 100, postal_code: 32 } as const;
  for (const field of Object.keys(limits) as (keyof CheckoutFields)[]) {
    if (actor === 'customer' && ['email', 'name', 'phone'].includes(field)) continue;
    if (fields[field].length > limits[field]) errors[field] = 'tooLong';
  }
  for (const field of ['country', 'address1', 'city', 'postal_code', ...(actor === 'guest' ? ['email' as const] : [])] as const) {
    if (!fields[field].trim()) errors[field] = 'required';
  }
  if (fields.country.trim() && !/^[A-Z]{2}$/.test(fields.country)) errors.country = 'country';
  if (actor === 'guest' && fields.email.trim() && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(fields.email.trim())) errors.email = 'email';
  return errors;
}
export function checkoutInput(fields: CheckoutFields, actor: PurchaseContext['actor'] = 'guest'): CheckoutInput {
  return {
    shipping_address: {
      country: fields.country, address1: fields.address1.trim(), city: fields.city.trim(), postal_code: fields.postal_code.trim(),
      ...(fields.address2.trim() && { address2: fields.address2.trim() }),
      ...(fields.subdivision.trim() && { subdivision: fields.subdivision.trim() }),
    },
    ...(actor === 'guest' && { buyer_contact: {
      email: fields.email.trim(), ...(fields.name.trim() && { name: fields.name.trim() }), ...(fields.phone.trim() && { phone: fields.phone.trim() }),
    } }),
  };
}
