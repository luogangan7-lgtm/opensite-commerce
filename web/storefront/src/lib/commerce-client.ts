/** Responsibility: same-origin browser purchasing requests; depends on browser-safe DTOs.
 * Callers: storefront React islands; invariants: no capability/amount calculation, no automatic write retries.
 */
import type { CartEnvelope, CheckoutInput, CheckoutResult, PaymentOutcome, Preview, PurchaseContext, ShippingAddress } from './commerce-contract';
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
    try { const problem=await response.json(); if(typeof problem.code==='string' && /^[a-z][a-z0-9_]{0,63}$/.test(problem.code))code=problem.code; } catch { /* non-JSON errors stay generic */ }
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
export const startCheckout = (input: CheckoutInput, key: string, csrf: string) => purchaseRequest<CheckoutResult>('checkout', { method: 'POST', body: input, key, csrf });
export const confirmPayment = (checkout_id: string, outcome: PaymentOutcome, csrf: string) => purchaseRequest<CheckoutResult>('payment', { method: 'POST', body: { checkout_id, outcome }, csrf });
export const getCompletion = () => purchaseRequest<CheckoutResult | null>('completion');
export const customerSession = (email: string, password: string, csrf: string) => purchaseRequest<PurchaseContext>('customer/session', { method: 'POST', body: { email, password }, csrf });
export const registerCustomer = (email: string, password: string, csrf: string) => purchaseRequest<{ status: 'verification_pending' }>('customer/register', { method: 'POST', body: { email, password }, csrf });
export const logoutCustomer = (csrf: string) => purchaseRequest<PurchaseContext>('customer/session', { method: 'DELETE', csrf });

export const confirmFixturePayment = confirmPayment;

/** Only stable public codes select typed local copy; upstream detail is never UI text. */
export function purchaseErrorMessage(error: unknown, copy: { requestFailed: string; stockUnavailable: string; priceChanged: string; checkoutBusy: string }): string {
  if (!(error instanceof CommerceError)) return copy.requestFailed;
  if (['stock_unavailable','inventory_insufficient'].includes(error.code)) return copy.stockUnavailable;
  if (['price_changed','pricing_changed','cart_changed'].includes(error.code)) return copy.priceChanged;
  if (['idempotency_in_progress','checkout_conflict'].includes(error.code)) return copy.checkoutBusy;
  return copy.requestFailed;
}
