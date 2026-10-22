/** Responsibility: same-origin browser purchasing requests; depends on browser-safe DTOs.
 * Callers: storefront React islands; invariants: no capability/amount calculation, no automatic write retries.
 */
import type { AcceptedTotal, CartEnvelope, CheckoutInput, CheckoutResult, PaymentOutcome, Preview, PurchaseContext, ShippingAddress } from './commerce-contract';
export class CommerceError extends Error {
  constructor(public readonly status: number, public readonly code = 'purchase_request_failed') { super('purchase_request_failed'); }
}
export async function purchaseRequest<T>(operation: string, options: { method?: string; body?: unknown; csrf?: string; etag?: string; key?: string } = {}): Promise<T> {
  const headers = new Headers({ Accept: 'application/json' });
  if (options.body !== undefined) headers.set('Content-Type', 'application/json');
  if (options.csrf) headers.set('X-CSRF-Token', options.csrf);
  if (options.etag) headers.set('If-Match', options.etag);
  if (options.key) headers.set('Idempotency-Key', options.key);
  const response = await fetch(`/purchase/api/${operation}`, { method: options.method ?? 'GET', headers,
    ...(options.body !== undefined ? { body: JSON.stringify(options.body) } : {}), credentials: 'same-origin', cache: 'no-store', redirect: 'error' });
  if (!response.ok) {
    let code='purchase_request_failed';
    try { if ((await response.json()).code==='checkout_total_changed') code='checkout_total_changed'; } catch { /* bounded fallback */ }
    throw new CommerceError(response.status,code);
  }
  return await response.json() as T;
}
export const getPurchaseContext = () => purchaseRequest<PurchaseContext>('context');
export const getCart = () => purchaseRequest<CartEnvelope | null>('cart');
export const addCartLine = (sku_id: string, quantity: number, csrf: string) => purchaseRequest<CartEnvelope>('cart', { method: 'POST', body: { sku_id, quantity }, csrf });
export const setCartLine = (sku_id: string, quantity: number, etag: string, csrf: string) => purchaseRequest<CartEnvelope>(`lines/${encodeURIComponent(sku_id)}`, { method: 'PUT', body: { quantity }, csrf, etag });
export const removeCartLine = (sku_id: string, etag: string, csrf: string) => purchaseRequest<CartEnvelope>(`lines/${encodeURIComponent(sku_id)}`, { method: 'DELETE', csrf, etag });
export const previewCheckout = (shipping_address: ShippingAddress, csrf: string) => purchaseRequest<Preview>('preview', { method: 'POST', body: { shipping_address }, csrf });
export const startCheckout = (input: CheckoutInput, key: string, csrf: string, accepted_total?: AcceptedTotal) => purchaseRequest<CheckoutResult>('checkout', { method: 'POST', body: {...input,accepted_total}, key, csrf });
export const confirmFixturePayment = (checkout_id: string, outcome: PaymentOutcome, csrf: string, accepted_total?: AcceptedTotal) => purchaseRequest<CheckoutResult>('payment', { method: 'POST', body: { checkout_id, outcome, accepted_total }, csrf });
export const getCompletion = () => purchaseRequest<CheckoutResult | null>('completion');
export const customerSession = (email: string, password: string, csrf: string) => purchaseRequest<PurchaseContext>('customer/session', { method: 'POST', body: { email, password }, csrf });
export const registerCustomer = (email: string, password: string, csrf: string) => purchaseRequest<{ status: 'verification_pending' }>('customer/register', { method: 'POST', body: { email, password }, csrf });
export const logoutCustomer = (csrf: string) => purchaseRequest<PurchaseContext>('customer/session', { method: 'DELETE', csrf });
