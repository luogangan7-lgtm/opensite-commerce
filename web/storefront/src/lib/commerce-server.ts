/** Responsibility: purchasing BFF and server-only capability custody.
 * Dependencies: Node crypto, Astro cookies, generated cart/checkout transport and explicit loopback fixture HTTP; callers: purchasing SSR/routes.
 * Invariants: explicit configured backend; no DB/staff credentials; session-bound CSRF and exact Origin precede writes;
 * capabilities only in signed Secure HttpOnly host cookie/upstream headers; money remains an int64 string.
 */
import { createHmac, randomBytes, timingSafeEqual } from 'node:crypto';
import type { AstroCookies } from 'astro';
import type { CartEnvelope, CheckoutResult, Preview, PurchaseContext, PurchaseProduct } from './commerce-contract';
import { minorAmount, parseIntegerJSON, safeInteger } from './commerce-wire';
import { readPublishedSite } from '@opensite/api-client';
import { readAPIOrigin } from './presentation';
import { requestCanonical } from './host-policy';
import { backendRequest } from './commerce-backend';
const COOKIE = '__Host-osc_storefront_fixture';
const key = randomBytes(32);
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const TOKEN = /^[A-Za-z0-9_-]{43}$/;
type Session = { s: string; c?: string; t?: string; tenant?: string; checkout?: string; x: number };
type Configuration = {origin: string; site: string; fixture?: boolean};
type Jar = Pick<AstroCookies, 'get' | 'set'>;
class HTTPError extends Error { constructor(readonly status: number, readonly code = 'purchase_request_failed') { super(code); } }
function signingKey() {
  if (process.env.OSC_COMMERCE_MODE !== 'backend') return key;
  const encoded = process.env.OSC_COMMERCE_COOKIE_KEY ?? '';
  const decoded = Buffer.from(encoded,'base64url');
  if (decoded.length < 32 || decoded.toString('base64url') !== encoded) throw new Error('Invalid cookie configuration');
  return decoded;
}
function cookieName() { return process.env.OSC_COMMERCE_MODE === 'backend' ? '__Host-osc_storefront_cart' : COOKIE; }
function mac(value: string) { return createHmac('sha256', signingKey()).update(value).digest('base64url'); }
function equal(a: string, b: string) { const x=Buffer.from(a), y=Buffer.from(b); return x.length === y.length && timingSafeEqual(x,y); }
export function fixtureConfiguration(request: Request): { origin: string; site: string } | null {
  const origin = process.env.OSC_COMMERCE_FIXTURE_ORIGIN, site = process.env.OSC_COMMERCE_FIXTURE_SITE;
  if (['forwarded','x-forwarded-host','x-forwarded-proto','x-forwarded-port'].some(header => request.headers.has(header))) return null;
  if (request.headers.has('Host') && request.headers.get('Host') !== '127.0.0.1:4323') return null;
  if (process.env.OSC_COMMERCE_FIXTURE_MODE !== 'contract-test' || origin !== 'http://127.0.0.1:4176' || site !== 'http://127.0.0.1:4323' || new URL(request.url).origin !== site) return null;
  return { origin, site };
}
function purchaseConfiguration(request: Request): Configuration | null {
  if (process.env.OSC_COMMERCE_MODE !== 'backend') {
    const fixture = fixtureConfiguration(request); return fixture ? {...fixture,fixture:true} : null;
  }
  try {
    const site = readAPIOrigin(process.env.OSC_COMMERCE_SITE ?? '');
    const url = new URL(request.url);
    if (url.origin !== site || !requestCanonical({url,request}) ||
        (url.protocol !== 'https:' && !(process.env.GITHUB_ACTIONS === 'true' && ['127.0.0.1','localhost'].includes(url.hostname)))) return null;
    signingKey();
    return {site,origin:readAPIOrigin(process.env.OSC_COMMERCE_API_ORIGIN ?? '')};
  } catch { return null; }
}
function readSession(cookies: Jar): Session | null {
  const raw = cookies.get(cookieName())?.value;
  if (!raw || raw.length > 2048) return null;
  const [encoded, signature, extra] = raw.split('.');
  if (!encoded || !signature || extra || !equal(signature, mac(encoded))) return null;
  try {
    const session = JSON.parse(Buffer.from(encoded, 'base64url').toString()) as Session;
    if (!TOKEN.test(session.s) || !Number.isSafeInteger(session.x) || session.x <= Date.now() || session.x > Date.now() + 3_600_000 ||
        (session.tenant !== undefined && !UUID.test(session.tenant)) || (session.checkout !== undefined && !UUID.test(session.checkout)) || (session.c !== undefined && !UUID.test(session.c)) || (session.t !== undefined && !TOKEN.test(session.t)) || !!session.c !== !!session.t) return null;
    return session;
  } catch { return null; }
}
function writeSession(cookies: Jar, session: Session) {
  const encoded = Buffer.from(JSON.stringify(session)).toString('base64url');
  cookies.set(cookieName(), `${encoded}.${mac(encoded)}`, { httpOnly: true, secure: true, sameSite: 'lax', path: '/', maxAge: 3600 });
}
function csrf(session: Session) { return mac(`csrf:${session.s}`); }
export function verifyPurchaseWrite(request: Request, site: string, token: string) {
  const received = request.headers.get('X-CSRF-Token') ?? '';
  if (request.headers.get('Origin') !== site || !equal(received, token)) throw new HTTPError(403);
  if (request.method !== 'DELETE' && request.headers.get('Content-Type')?.split(';')[0]?.trim() !== 'application/json') throw new HTTPError(415);
}
function object(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new HTTPError(502);
  return value as Record<string, unknown>;
}
function string(value: unknown, max = 256) { if (typeof value !== 'string' || !value.length || value.length > max) throw new HTTPError(502); return value; }
function uuid(value: unknown) { const id = string(value); if (!UUID.test(id)) throw new HTTPError(502); return id; }
function currency(value: unknown) { const code = string(value); if (!/^[A-Z]{3}$/.test(code)) throw new HTTPError(502); return code; }
function configuredKey(name: string) {
  const encoded=process.env[name] ?? '', decoded=Buffer.from(encoded,'base64url');
  if (decoded.length<32 || decoded.toString('base64url')!==encoded) throw new HTTPError(503);
  return decoded;
}
function fakePaymentEnabled() {
  return process.env.OPENSITE_FAKE_PSP==='1' && ['test','development'].includes(process.env.OPENSITE_ENVIRONMENT ?? '');
}
function checkoutHeaders(configuration: Configuration, session: Session): Record<string,string> {
  if (configuration.fixture) return {Origin:configuration.site,'X-CSRF-Token':csrf(session)};
  const origin=readAPIOrigin(process.env.OPENSITE_STOREFRONT_ORIGIN ?? '');
  if (!origin.startsWith('https://') || !session.t || !session.c || !session.tenant) throw new HTTPError(503);
  const proof=createHmac('sha256',configuredKey('OPENSITE_STOREFRONT_KEY')).update(`checkout:${session.tenant}:${session.t}:${origin}`).digest('base64url');
  return {Origin:origin,'X-CSRF-Token':proof,'X-Cart-ID':session.c};
}
async function boundedText(response: Response) {
  const reader = response.body?.getReader();
  if (!reader) throw new HTTPError(502);
  const chunks: Uint8Array[] = []; let size = 0;
  while (true) { const next = await reader.read(); if (next.done) break; size += next.value.byteLength;
    if (size > 1_048_576) { await reader.cancel(); throw new HTTPError(502); } chunks.push(next.value); }
  return Buffer.concat(chunks).toString('utf8');
}
async function upstream(configuration: Configuration, session: Session, path: string, options: { method?: string; body?: unknown; rawBody?: string; headers?: Record<string,string> } = {}) {
  let response: Response, text: string;
  if (!configuration.fixture) {
    const result = await backendRequest(configuration.origin,path,session.t,{...options,headers:{...(path.includes('/checkout') ? checkoutHeaders(configuration,session) : {}),...options.headers}});
    response=result.response; text=result.text;
  } else {
    response=await fetch(`${configuration.origin}${path}`, {method:options.method ?? 'GET',redirect:'error',signal:AbortSignal.timeout(5000),
      headers:{Accept:'application/json','X-Fixture-Session':session.s,...(session.t ? {'X-Cart-Token':session.t}:{}),...(options.body !== undefined ? {'Content-Type':'application/json'}:{}),...options.headers},
      ...(options.body !== undefined ? {body:JSON.stringify(options.body)}:{})});
    text=await boundedText(response);
  }
  if (!response.ok) {
    let code='purchase_request_failed';
    // Codes are stable public vocabulary; never relay raw detail/title/instance.
    try { const problem=JSON.parse(text); if (typeof problem.code==='string' && /^[a-z][a-z0-9_]{0,63}$/.test(problem.code)) code=problem.code; } catch { /* generic failure */ }
    throw new HTTPError([400,401,403,404,409,412,413,415,422,428,429,503].includes(response.status) ? response.status : 502,code);
  }
  return {value:parseIntegerJSON(text),etag:response.headers.get('ETag')};
}
function products(value: unknown): PurchaseProduct[] {
  if (!Array.isArray(value) || value.length > 100) throw new HTTPError(502);
  return value.map(raw => {
    const product = object(raw);
    if (!Array.isArray(product.skus) || product.skus.length > 100) throw new HTTPError(502);
    return { product_id: uuid(product.product_id), name: string(product.name), slug: string(product.slug), skus: product.skus.map(rawSKU => {
      const sku = object(rawSKU);
      if (typeof sku.available !== 'boolean' || !Array.isArray(sku.option_values) || sku.option_values.length > 20) throw new HTTPError(502);
      return { sku_id: uuid(sku.sku_id), sku_code: string(sku.sku_code), price_minor: minorAmount(sku.price_minor), currency: currency(sku.currency), available: sku.available,
        option_values: sku.option_values.map(rawOption => { const option = object(rawOption); return { name: string(option.name), value: string(option.value) }; }) };
    }) };
  });
}
async function context(configuration: Configuration, session: Session) {
  if (configuration.fixture) {
    const raw = object((await upstream(configuration,session,'/__fixture/context')).value);
    if (raw.actor !== 'guest' && raw.actor !== 'customer') throw new HTTPError(502);
    return {tenant:uuid(raw.tenant_id),view:{enabled:true,fixture:true,csrf:csrf(session),actor:raw.actor,products:products(raw.products)} satisfies PurchaseContext};
  }
  const raw = object(await readPublishedSite(configuration.origin,new URL(configuration.site).hostname));
  const tenant=uuid(raw.tenant_id);
  if (session.tenant && session.tenant !== tenant) throw new HTTPError(404);
  session.tenant=tenant;
  if (!Array.isArray(raw.products)) throw new HTTPError(502);
  const catalogue=products(raw.products.map(value=>{const product=object(value);return {...product,product_id:product.id};}));
  return {tenant,view:{enabled:true,fixture:false,csrf:csrf(session),actor:'guest',products:catalogue,...(fakePaymentEnabled() ? {fakePayment:true} : {})} satisfies PurchaseContext};
}
const disabled: PurchaseContext = { enabled: false, fixture: false, csrf: '', actor: 'guest', products: [] };
export async function readPurchaseContext(request: Request, cookies: Jar): Promise<PurchaseContext> {
  const config=purchaseConfiguration(request);
  if (!config) return disabled;
  try {
    const session=readSession(cookies) ?? {s:randomBytes(32).toString('base64url'),x:Date.now()+3_600_000};
    const result=await context(config,session);writeSession(cookies,session);return result.view;
  } catch { return {...disabled,fixture:!!config.fixture}; }
}
function cartEnvelope(value: unknown, etag: string | null, catalogue: PurchaseProduct[]): CartEnvelope {
  const raw = object(value);
  if (!etag || !/^"v\d+-[0-9a-f]{64}"$/.test(etag) || !Array.isArray(raw.lines) || raw.lines.length > 500) throw new HTTPError(502);
  const code = currency(raw.currency);
  const lines = raw.lines.map(rawLine => {
    const line = object(rawLine), sku_id = uuid(line.sku_id);
    const product = catalogue.find(product => product.skus.some(sku => sku.sku_id === sku_id));
    const sku = product?.skus.find(sku => sku.sku_id === sku_id);
    if (!sku || typeof line.sellable !== 'boolean') throw new HTTPError(502);
    return { sku_id, name: product!.name, sku_code: sku.sku_code, quantity: safeInteger(line.quantity, 1, 1_000_000), sellable: line.sellable,
      ...(line.pricing_error ? { pricing_error: string(line.pricing_error) } : { unit_amount: minorAmount(line.unit_amount), line_amount: minorAmount(line.line_amount) }) };
  });
  return { etag, cart: { id: uuid(raw.id), currency: code, version: minorAmount(raw.version), lines,
    ...(raw.pricing_error ? { pricing_error: string(raw.pricing_error) } : { subtotal: minorAmount(raw.subtotal) }) } };
}
function preview(value: unknown): Preview {
  const raw = object(value);
  if (!Array.isArray(raw.shipping_options) || raw.shipping_options.length > 20) throw new HTTPError(502);
  return { subtotal_minor: minorAmount(raw.subtotal_minor), tax_minor: minorAmount(raw.tax_minor), shipping_minor: minorAmount(raw.shipping_minor), total_minor: minorAmount(raw.total_minor), currency: currency(raw.currency),
    shipping_options: raw.shipping_options.map(value => { const item = object(value); return { rate_id: string(item.rate_id), method: string(item.method), amount_minor: minorAmount(item.amount_minor) }; }) };
}
function checkout(value: unknown): CheckoutResult {
  const raw = object(value), payment = object(raw.payment);
  return { checkout_id: uuid(raw.checkout_id), order_id: uuid(raw.order_id), state: string(raw.state), amount_minor: minorAmount(raw.amount_minor), currency: currency(raw.currency), order_version: minorAmount(raw.order_version), payment: { provider_ref: string(payment.provider_ref), status: string(payment.status) } };
}
function paymentReceipt(value: unknown, eventID?: string) {
  const raw=object(value);
  if (Object.keys(raw).some(key=>!['event_id','status'].includes(key)) ||
      !string(raw.event_id,128) || (eventID !== undefined && raw.event_id !== eventID) ||
      !['pending','processed','rejected'].includes(String(raw.status))) throw new HTTPError(502);
  if (raw.status === 'rejected') throw new HTTPError(409,'payment_conflict');
}
async function body(request: Request, allowed: readonly string[]) {
  if (Number(request.headers.get('Content-Length') ?? '0') > 16_384) throw new HTTPError(413);
  const reader = request.body?.getReader(); if (!reader) throw new HTTPError(400);
  let length = 0; const chunks: Uint8Array[] = [];
  while (true) { const next = await reader.read(); if (next.done) break; length += next.value.byteLength;
    if (length > 16_384) { await reader.cancel(); throw new HTTPError(413); } chunks.push(next.value); }
  let raw: Record<string,unknown>;
  try { raw = object(JSON.parse(Buffer.concat(chunks).toString('utf8'))); } catch { throw new HTTPError(400); }
  if (Object.keys(raw).some(key => !allowed.includes(key))) throw new HTTPError(400);
  return raw;
}
function quantity(value: unknown) { if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0 || value > 1_000_000) throw new HTTPError(400); return value; }
function json(value: unknown, status = 200) { return new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json', 'Cache-Control': 'no-store', 'X-Robots-Tag': 'noindex' } }); }
export async function handlePurchaseRequest(request: Request, cookies: Jar, operation: string): Promise<Response> {
  const config = purchaseConfiguration(request);
  if (!config) return request.method === 'GET' && operation === 'context' ? json(disabled) : json({ code: 'purchase_unavailable' }, 503);
  const configuration = config;
  try {
    if (request.method === 'GET' && operation === 'context') return json(await readPurchaseContext(request, cookies));
    const session = readSession(cookies); if (!session) throw new HTTPError(configuration.fixture ? 403 : 401);
    if (!['GET','HEAD'].includes(request.method)) verifyPurchaseWrite(request, configuration.site, csrf(session));
    const current = await context(configuration, session), base = `/api/v1/t/${current.tenant}`;
    if (operation === 'cart' && request.method === 'GET') {
      if (!session.c) return json(null);
      const response = await upstream(configuration, session, `${base}/carts/${session.c}`);
      return json(cartEnvelope(response.value, response.etag, current.view.products));
    }
    if (operation === 'cart' && request.method === 'POST') {
      const input = await body(request, ['sku_id','quantity']); const sku_id = uuid(input.sku_id), amount = quantity(input.quantity);
      const sku = current.view.products.flatMap(product => product.skus).find(sku => sku.sku_id === sku_id);
      if (!sku?.available || amount < 1) throw new HTTPError(422);
      if (!session.c) {
        const created = object((await upstream(configuration, session, `${base}/carts`, { method: 'POST', body: { currency: sku.currency } })).value);
        session.c = uuid(object(created.cart).id); session.t = string(created.cart_token);
        if (!TOKEN.test(session.t)) throw new HTTPError(502);
        // Persist the capability before a subsequent line write: an ambiguous write is never retried as cart creation.
        writeSession(cookies, session);
      }
      const latest = await upstream(configuration, session, `${base}/carts/${session.c}`);
      const envelope = cartEnvelope(latest.value, latest.etag, current.view.products);
      const response = await upstream(configuration, session, `${base}/carts/${session.c}/lines/${sku_id}`, { method: 'PUT', body: { quantity: amount }, headers: { 'If-Match': envelope.etag } });
      return json(cartEnvelope(response.value, response.etag, current.view.products));
    }
    if (operation.startsWith('lines/') && ['PUT','DELETE'].includes(request.method)) {
      if (!session.c) throw new HTTPError(404);
      const id = uuid(operation.slice(6)), etag = request.headers.get('If-Match'); if (!etag) throw new HTTPError(428);
      if (!/^"v\d+-[0-9a-f]{64}"$/.test(etag)) throw new HTTPError(400);
      const input = request.method === 'PUT' ? await body(request, ['quantity']) : null;
      const response = await upstream(configuration, session, `${base}/carts/${session.c}/lines/${id}`, { method: request.method,
        ...(input ? { body: { quantity: quantity(input.quantity) } } : {}), headers: { 'If-Match': etag } });
      return json(cartEnvelope(response.value, response.etag, current.view.products));
    }
    if (operation === 'preview' && request.method === 'POST') {
      if (!session.c) throw new HTTPError(404);
      const input = await body(request, ['shipping_address']), address = object(input.shipping_address);
      const response = await upstream(configuration, session, `${base}/checkout/preview`, { method: 'POST', body: { cart_id: session.c, shipping_address: { country: address.country, ...(address.subdivision ? { region: address.subdivision } : {}), ...(address.postal_code ? { postal_code: address.postal_code } : {}) } } });
      return json(preview(response.value));
    }
    if (operation === 'checkout' && request.method === 'POST') {
      if (!session.c) throw new HTTPError(404);
      const idempotency = request.headers.get('Idempotency-Key'); if (!idempotency || !/^[!-~]{1,128}$/.test(idempotency)) throw new HTTPError(400);
      const input = await body(request, ['shipping_address','buyer_contact']);
      const response = await upstream(configuration, session, `${base}/checkout`, { method: 'POST', body: { ...input, cart_id: session.c }, headers: { 'Idempotency-Key': idempotency,...checkoutHeaders(configuration,session) } });
      const result=checkout(response.value);session.checkout=result.checkout_id;writeSession(cookies,session);return json(result,201);
    }
    if (operation === 'payment' && request.method === 'POST') {
      const input = await body(request, ['checkout_id','outcome']); uuid(input.checkout_id);
      if (!['captured','failed','canceled'].includes(String(input.outcome))) throw new HTTPError(400);
      if (configuration.fixture) {
        paymentReceipt((await upstream(configuration,session,'/__fixture/payment',{method:'POST',body:input})).value);
        return json(checkout((await upstream(configuration,session,'/__fixture/completion')).value));
      }
      if (!fakePaymentEnabled() || !session.checkout || input.checkout_id!==session.checkout) throw new HTTPError(404);
      const current=checkout((await upstream(configuration,session,`${base}/checkout/${session.checkout}`)).value);
      if (['captured','failed','canceled'].includes(current.payment.status)) return json(current);
      // Authoritative backend state supplies every event field; the browser chooses only a test outcome.
      const account=process.env.OPENSITE_FAKE_ACCOUNT_ID ?? '';
      if (!/^[A-Za-z0-9_-]{1,128}$/.test(account)) throw new HTTPError(503);
      const event={event_id:`c09d:${current.checkout_id}:${input.outcome}`,account_id:account,order_id:current.order_id,
        provider_ref:current.payment.provider_ref,status:input.outcome,currency:current.currency};
      const raw=JSON.stringify(event).slice(0,-1)+`,"amount_minor":${minorAmount(current.amount_minor)},"order_version":${minorAmount(current.order_version)}}`;
      // Node supplies a timestamp; only Payments' DB clock may admit its freshness.
      const stamp=String(Math.floor(Date.now()/1000));
      const signature=`t=${stamp},v1=${createHmac('sha256',configuredKey('OPENSITE_FAKE_WEBHOOK_KEY')).update(`${stamp}.${raw}`).digest('hex')}`;
      const receipt=await upstream(configuration,{s:session.s,x:session.x},'/api/v1/testing/payments/fake/webhook',
        {method:'POST',body:event,rawBody:raw,headers:{'X-Fake-Signature':signature}});
      paymentReceipt(receipt.value,event.event_id);
      return json(checkout((await upstream(configuration,session,`${base}/checkout/${session.checkout}`)).value));
    }
    if (operation === 'completion' && request.method === 'GET') {
      if (!configuration.fixture && !session.checkout) return json(null);
      const value=(await upstream(configuration,session,configuration.fixture ? '/__fixture/completion' : `${base}/checkout/${session.checkout}`)).value;return json(value===null?null:checkout(value));
    }
    if (configuration.fixture && operation === 'customer/session' && ['POST','DELETE'].includes(request.method)) {
      const input = request.method === 'POST' ? await body(request, ['email','password']) : undefined;
      await upstream(configuration, session, `${base}/customer/session`, { method: request.method, ...(input ? { body: input } : {}) });
      return json((await context(configuration, session)).view);
    }
    if (configuration.fixture && operation === 'customer/register' && request.method === 'POST') {
      const input = await body(request, ['email','password']);
      await upstream(configuration, session, '/__fixture/register', { method: 'POST', body: input });
      return json({ status: 'verification_pending' });
    }
    throw new HTTPError(404);
  } catch (error) { const status=error instanceof HTTPError ? error.status : 502;
    if (configuration.fixture) return json({code:'purchase_request_failed'},status);
    return new Response(JSON.stringify({type:'about:blank',status,code:error instanceof HTTPError ? error.code : 'purchase_request_failed'}),{status,headers:{'Content-Type':'application/problem+json','Cache-Control':'no-store','X-Robots-Tag':'noindex'}});
  }
}
