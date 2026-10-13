/** Responsibility: explicitly synthetic C-09c upstream contracts, never a production purchasing service.
 * Dependencies: Node HTTP/crypto only and browser-safe purchase types; callers: SSR BFF + Playwright.
 * Invariants: loopback binding, private session/capability ownership, server-computed money and strong ETags.
 * ponytail: ephemeral maps model contracts, not authentication/payment durability; replace with real APIs at handoff.
 */
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { randomBytes, randomUUID, createHash } from 'node:crypto';
import type { PurchaseProduct } from '../../web/storefront/src/lib/commerce-contract';
const tenant = 'c0900001-0000-4000-8000-000000000001';
const sku = 'c09c0001-0000-4000-8000-000000000001';
const product: PurchaseProduct = { product_id: 'c0900007-0000-4000-8000-000000000001', name: 'Canvas tote', slug: 'canvas-tote', skus: [
  { sku_id: sku, sku_code: 'TOTE-NATURAL', option_values: [{ name: 'Color', value: 'Natural' }], price_minor: '2400', currency: 'USD', available: true },
  { sku_id: 'c09c0001-0000-4000-8000-000000000002', sku_code: 'TOTE-BLACK', option_values: [{ name: 'Color', value: 'Black' }], price_minor: '2400', currency: 'USD', available: false },
] };
type Cart = { id: string; currency: string; version: number; lines: { sku_id: string; quantity: number; unit_amount: number; line_amount: number; available_quantity: number; sellable: boolean; price_list_id: string; priority: number; min_quantity: number }[]; subtotal: number };
type Checkout = { checkout_id: string; order_id: string; state: string; amount_minor: number; currency: string; order_version: number; payment: { provider_ref: string; status: string } };
type Session = { actor: 'guest' | 'customer'; cart?: Cart; token?: string; checkout?: Checkout; idempotency: Map<string, { fingerprint: string; result: Checkout }> };
const sessions = new Map<string, Session>();
const etag = (cart: Cart) => `"v${cart.version}-${createHash('sha256').update(JSON.stringify(cart)).digest('hex')}"`;
function reply(res: ServerResponse, status: number, data?: unknown, tag?: string) {
  res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store', ...(tag ? { ETag: tag } : {}) });
  res.end(data === undefined ? undefined : JSON.stringify(data));
}
function reject(res: ServerResponse, status: number, code: string) { reply(res, status, { code, detail: `Contract fixture: ${code}` }); }
async function body(req: IncomingMessage): Promise<Record<string, unknown>> {
  let data = ''; for await (const part of req) { data += part; if (Buffer.byteLength(data) > 16_384) throw new Error('body_too_large'); }
  if (!data) return {};
  const parsed: unknown = JSON.parse(data);
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('invalid_json');
  return parsed as Record<string, unknown>;
}
function totals(cart: Cart) {
  return { subtotal_minor: String(cart.subtotal), tax_minor: String(cart.subtotal / 10), shipping_minor: '500', total_minor: String(cart.subtotal + cart.subtotal / 10 + 500), currency: 'USD', shipping_options: [{ rate_id: 'fixture-standard', method: 'Standard test shipping', amount_minor: '500' }] };
}
function shipping(value: unknown): boolean {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const address = value as Record<string, unknown>;
  return typeof address.country === 'string' && /^[A-Z]{2}$/.test(address.country) && (address.postal_code === undefined || (typeof address.postal_code === 'string' && address.postal_code.length <= 32));
}
function checkoutAddress(value: unknown): boolean {
  if (!shipping(value)) return false;
  const address = value as Record<string, unknown>;
  const limits: Record<string, number> = { country: 2, address1: 256, address2: 256, city: 128, subdivision: 100, postal_code: 32 };
  if (Object.keys(address).some(key => !(key in limits))) return false;
  if (['address1', 'city', 'postal_code'].some(key => typeof address[key] !== 'string' || !String(address[key]).trim())) return false;
  return Object.entries(address).every(([key, value]) => typeof value === 'string' && value.length <= limits[key]);
}
function guestContact(value: unknown): boolean {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const contact = value as Record<string, unknown>;
  return typeof contact.email === 'string' && contact.email.length <= 254 && /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(contact.email) &&
    Object.entries(contact).every(([key, value]) => ['email', 'name', 'phone'].includes(key) && typeof value === 'string' && value.length <= (key === 'phone' ? 64 : key === 'name' ? 200 : 254));
}
export const commerceFixtureServer = createServer(async (req, res) => {
  try {
    const path = new URL(req.url ?? '/', 'http://127.0.0.1').pathname;
    const method = req.method ?? 'GET';
    if (path === '/healthz' && method === 'GET') return reply(res, 200, { fixture: true });
    if (req.headers.origin && req.headers.origin !== 'http://127.0.0.1:4323') return reject(res, 403, 'origin_rejected');
    // Node-only test controller: browsers send Origin for cross-origin POST; no capability is returned.
    if (path === '/__fixture/scenario' && method === 'POST') {
      if (req.headers.origin || !['127.0.0.1', '::1', '::ffff:127.0.0.1'].includes(req.socket.remoteAddress ?? '')) return reject(res, 403, 'controller_rejected');
      const input = await body(req); const session = [...sessions.values()].find(value => value.cart?.id === input.cart_id);
      if (!session?.cart || input.action !== 'conflict') return reject(res, 404, 'scenario_not_found');
      session.cart.lines = session.cart.lines.map(line => ({ ...line, quantity: 2, line_amount: 4800 })); session.cart.subtotal = session.cart.lines.reduce((sum, line) => sum + line.line_amount, 0);
      session.cart.version++; return reply(res, 200, { applied: true });
    }
    // Browser calls cannot supply this server-only session. Root BFF never serializes it.
    const sessionID = req.headers['x-fixture-session'];
    if (typeof sessionID !== 'string' || !/^[A-Za-z0-9_-]{43}$/.test(sessionID)) return reject(res, 401, 'fixture_session_required');
    let session = sessions.get(sessionID);
    if (!session) { session = { actor: 'guest', idempotency: new Map() }; sessions.set(sessionID, session); }
    if (path === '/__fixture/context' && method === 'GET') return reply(res, 200, { tenant_id: tenant, actor: session.actor, products: [product] });
    if (path === '/__fixture/register' && method === 'POST') {
      const input = await body(req);
      if (typeof input.email !== 'string' || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(input.email) || typeof input.password !== 'string' || input.password.length < 8) return reject(res, 422, 'registration_invalid');
      return reply(res, 202, { state: 'verification_pending', fixture: true });
    }
    const prefix = `/api/v1/t/${tenant}`;
    if (path === `${prefix}/customer/session`) {
      if (method === 'POST') { const input = await body(req); if (typeof input.email !== 'string' || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(input.email) || typeof input.password !== 'string' || input.password.length < 8) return reject(res, 401, 'credentials_invalid'); session.actor = 'customer'; return reply(res, 200, { actor: 'customer', fixture: true }); }
      if (method === 'DELETE') { session.actor = 'guest'; return reply(res, 200, { actor: 'guest' }); }
    }
    if (path === `${prefix}/carts` && method === 'POST') {
      const input = await body(req); if (input.currency !== 'USD') return reject(res, 422, 'currency_invalid');
      if (!session.cart) { session.cart = { id: randomUUID(), currency: 'USD', version: 1, lines: [], subtotal: 0 }; session.token = randomBytes(32).toString('base64url'); }
      return reply(res, 201, { cart: session.cart, cart_token: session.token }, etag(session.cart));
    }
    if (!session.cart || req.headers['x-cart-token'] !== session.token) return reject(res, 404, 'cart_not_found');
    const cart = session.cart;
    if (path === `${prefix}/carts/${cart.id}` && method === 'GET') return reply(res, 200, cart, etag(cart));
    if (path === `${prefix}/carts/${cart.id}/lines/${sku}` && ['PUT', 'DELETE'].includes(method)) {
      if (!req.headers['if-match']) return reject(res, 428, 'if_match_required');
      if (req.headers['if-match'] !== etag(cart)) return reject(res, 412, 'precondition_failed');
      const input = method === 'DELETE' ? { quantity: 0 } : await body(req);
      if (!Number.isInteger(input.quantity) || Number(input.quantity) < 0 || Number(input.quantity) > 1_000_000) return reject(res, 422, 'quantity_invalid');
      const quantity = Number(input.quantity);
      cart.lines = quantity === 0 ? [] : [{ sku_id: sku, quantity, unit_amount: 2400, line_amount: quantity * 2400, available_quantity: 1000000, sellable: true, price_list_id: 'c09c0002-0000-4000-8000-000000000001', priority: 0, min_quantity: 1 }];
      cart.subtotal = quantity * 2400; cart.version++;
      return reply(res, 200, cart, etag(cart));
    }
    if (path === `${prefix}/checkout/preview` && method === 'POST') {
      const input = await body(req); if (input.cart_id !== cart.id || !shipping(input.shipping_address)) return reject(res, 422, 'preview_invalid');
      if (!cart.lines.length) return reject(res, 422, 'cart_empty');
      return reply(res, 200, totals(cart));
    }
    if (path === `${prefix}/checkout` && method === 'POST') {
      const input = await body(req); const key = req.headers['idempotency-key'];
      if (typeof key !== 'string' || !/^[!-~]{1,128}$/.test(key)) return reject(res, 400, 'idempotency_key_required');
      const fingerprint = JSON.stringify(input); const previous = session.idempotency.get(key);
      if (previous) return previous.fingerprint === fingerprint ? reply(res, 200, previous.result) : reject(res, 422, 'idempotency_conflict');
      if (input.cart_id !== cart.id || !checkoutAddress(input.shipping_address) || !cart.lines.length) return reject(res, 422, 'checkout_invalid');
      if (session.actor === 'customer' && input.buyer_contact !== undefined) return reject(res, 422, 'buyer_contact_forbidden');
      if (session.actor === 'guest') { if (!guestContact(input.buyer_contact)) return reject(res, 422, 'buyer_contact_required'); }
      if (session.checkout?.state === 'paid') return reject(res, 409, 'cart_consumed');
      const ordinal = String(session.idempotency.size + 1).padStart(12, '0');
      const result: Checkout = { checkout_id: `c09c0010-0000-4000-8000-${ordinal}`, order_id: `c09c0011-0000-4000-8000-${ordinal}`, state: 'ready', amount_minor: Number(totals(cart).total_minor), currency: 'USD', order_version: 1, payment: { provider_ref: `fake:${ordinal}`, status: 'authorized' } };
      session.checkout = result; session.idempotency.set(key, { fingerprint, result: structuredClone(result) }); return reply(res, 201, result);
    }
    if (path === '/__fixture/payment' && method === 'POST') {
      const input = await body(req); const result = session.checkout;
      if (!result || input.checkout_id !== result.checkout_id) return reject(res, 404, 'checkout_not_found');
      if (!['captured', 'failed', 'canceled'].includes(String(input.outcome))) return reject(res, 422, 'payment_outcome_invalid');
      if (result.state === 'paid') return reply(res, 200, result);
      result.payment.status = String(input.outcome); result.state = input.outcome === 'captured' ? 'paid' : String(input.outcome); result.order_version++;
      return reply(res, 200, result);
    }
    if (path === '/__fixture/completion' && method === 'GET') return session.checkout ? reply(res, 200, session.checkout) : reject(res, 404, 'checkout_not_found');
    return reject(res, 404, 'route_not_found');
  } catch (error) { return reject(res, error instanceof Error && error.message === 'body_too_large' ? 413 : 400, 'invalid_request'); }
});
if (import.meta.main) commerceFixtureServer.listen(4176, '127.0.0.1');
