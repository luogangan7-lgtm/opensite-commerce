/** Responsibility: browser-safe purchasing view contracts for the approved C-09c fixtures.
 * Dependencies: none; callers: SSR adapter, React purchasing UI and contract fixtures.
 * Invariants: capabilities never appear here; money is an exact minor-unit string, computed by the API.
 * Backend handoff: SKU/preview/customer contracts are owner-approved proposals, not implemented API claims.
 */
export type PurchaseSKU = { sku_id: string; sku_code: string; option_values: { name: string; value: string }[]; price_minor: string; currency: string; available: boolean };
export type PurchaseProduct = { product_id: string; name: string; slug: string; skus: PurchaseSKU[] };
export type CartLine = { sku_id: string; name: string; sku_code: string; quantity: number; sellable: boolean; unit_amount?: string; line_amount?: string; pricing_error?: string };
export type CartView = { id: string; currency: string; version: string; lines: CartLine[]; subtotal?: string; pricing_error?: string };
export type CartEnvelope = { cart: CartView; etag: string };
export type PurchaseContext = { enabled: boolean; fixture: boolean; csrf: string; fakePayment?: boolean; actor: 'guest' | 'customer'; products: PurchaseProduct[] };
export type ShippingAddress = { country: string; address1: string; address2?: string; city: string; subdivision?: string; postal_code: string };
export type BuyerContact = { email: string; name?: string; phone?: string };
export type Preview = { subtotal_minor: string; tax_minor: string; shipping_minor: string; total_minor: string; currency: string; shipping_options: { rate_id: string; method: string; amount_minor: string }[] };
export type CheckoutInput = { shipping_address: ShippingAddress; buyer_contact?: BuyerContact };
export type CheckoutResult = { checkout_id: string; order_id: string; state: string; amount_minor: string; currency: string; order_version: string; payment: { provider_ref: string; status: string } };
export type PaymentOutcome = 'captured' | 'failed' | 'canceled';

export type AcceptedTotal = {amount_minor: string; currency: string};
