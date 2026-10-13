/** Responsibility: real-backend purchasing admission and lossless transport regressions.
 * Dependencies: public generated API contracts and Astro cookie seam; caller: CI Bun suite.
 * Invariants: no fixture dependency, cookie-only capability, exact Origin/CSRF, safe problem codes.
 */
import { afterEach, beforeEach, expect, spyOn, test } from 'bun:test';
import { randomBytes } from 'node:crypto';
import type { AstroCookies } from 'astro';
import { handlePurchaseRequest, readPurchaseContext } from './commerce-server';
const site = 'https://shop.example.test', tenant = 'c0900001-0000-4000-8000-000000000001';
const sku = 'c0900008-0000-4000-8000-000000000001', product = 'c0900007-0000-4000-8000-000000000001';
const cart = 'c09d0001-0000-4000-8000-000000000001', token = randomBytes(32).toString('base64url');
const etag = `"v1-${'a'.repeat(64)}"`;
const names = ['OSC_COMMERCE_MODE','OSC_COMMERCE_API_ORIGIN','OSC_COMMERCE_SITE','OSC_COMMERCE_COOKIE_KEY','OSC_STOREFRONT_ALLOWED_HOSTS'];
const saved = new Map(names.map(name => [name, process.env[name]]));
let mock: ReturnType<typeof spyOn>;
const calls: Request[] = [];
let failure = false;
function cookies() {
  const values = new Map<string,string>(), flags: Record<string,unknown>[] = [];
  return { jar: {get: (key: string) => values.has(key) ? {value: values.get(key)!} : undefined,
    set: (key: string, value: string, options: Record<string,unknown>) => { values.set(key,value); flags.push(options); }} as unknown as AstroCookies, values, flags };
}
function request(operation: string, csrf?: string, value?: unknown) {
  return new Request(`${site}/purchase/api/${operation}`, { headers: {Host: 'shop.example.test', ...(csrf ? {Origin: site,'X-CSRF-Token':csrf,'Content-Type':'application/json'} : {})},
    ...(value ? {method:'POST',body:JSON.stringify(value)} : {}) });
}
beforeEach(() => {
  Object.assign(process.env, {OSC_COMMERCE_MODE:'backend',OSC_COMMERCE_API_ORIGIN:'http://api:8080',OSC_COMMERCE_SITE:site,OSC_COMMERCE_COOKIE_KEY:randomBytes(32).toString('base64url'),OSC_STOREFRONT_ALLOWED_HOSTS:'shop.example.test'});
  calls.length=0; failure=false;
  mock=spyOn(globalThis,'fetch').mockImplementation(Object.assign(async(input:RequestInfo|URL,init?:RequestInit)=>{
    const req=input instanceof Request ? input : new Request(input,init); calls.push(req);
    if(new URL(req.url).pathname==='/api/v1/public/sites/current') return Response.json({tenant_id:tenant,hostname:'shop.example.test',locale:'en',products:[{id:product,name:'Canvas tote',slug:'canvas-tote',skus:[{sku_id:sku,sku_code:'TOTE',option_values:[],price_minor:'2400',currency:'USD',available:true}]}]});
    if(failure) return Response.json({type:'about:blank',status:409,code:'inventory_insufficient',detail:'postgres://private/internal'}, {status:409,headers:{'Content-Type':'application/problem+json'}});
    if(req.url.endsWith('/carts')) return Response.json({cart:{id:cart,currency:'USD',version:1,lines:[],subtotal:0},cart_token:token},{status:201,headers:{ETag:etag}});
    return new Response(`{"id":"${cart}","currency":"USD","version":1,"lines":[{"sku_id":"${sku}","quantity":1,"sellable":true,"unit_amount":9223372036854775807,"line_amount":9223372036854775807}],"subtotal":9223372036854775807}`,{headers:{ETag:etag,'Content-Type':'application/json'}});
  },{preconnect:globalThis.fetch.preconnect}));
});
afterEach(()=>{mock?.mockRestore();for(const name of names){const value=saved.get(name);if(value===undefined)delete process.env[name];else process.env[name]=value;}});
test('configured backend creates and reads the real cart with cookie-only capability and exact int64 amounts',async()=>{
  const jar=cookies(), context=await readPurchaseContext(request('context'),jar.jar);
  expect(context.enabled).toBe(true);expect(context.fixture).toBe(false);
  const response=await handlePurchaseRequest(request('cart',context.csrf,{sku_id:sku,quantity:1}),jar.jar,'cart');
  expect(response.status).toBe(200);const text=await response.text();
  expect(JSON.parse(text).cart.subtotal).toBe('9223372036854775807');expect(text).not.toContain(token);expect(text).not.toContain('cart_token');
  expect(calls.some(call=>call.url===`http://api:8080/api/v1/t/${tenant}/carts`&&call.method==='POST')).toBe(true);
  expect(calls.some(call=>call.headers.get('X-Cart-Token')===token)).toBe(true);
  expect(calls.every(call=>!call.url.includes('__fixture'))).toBe(true);
  expect(jar.flags.every(options=>options.httpOnly===true&&options.secure===true&&options.sameSite==='lax')).toBe(true);
});
test('backend problem code is visible without returning upstream private detail',async()=>{
  const jar=cookies(),context=await readPurchaseContext(request('context'),jar.jar);
  expect(context.enabled).toBe(true);
  await handlePurchaseRequest(request('cart',context.csrf,{sku_id:sku,quantity:1}),jar.jar,'cart');
  failure=true;
  const response=await handlePurchaseRequest(new Request(`${site}/purchase/api/checkout`,{method:'POST',headers:{Host:'shop.example.test',Origin:site,'X-CSRF-Token':context.csrf,'Content-Type':'application/json','Idempotency-Key':'c09d-retry-same-key'},body:JSON.stringify({shipping_address:{country:'US',address1:'1 Test Way',city:'New York',postal_code:'10001'},buyer_contact:{email:'buyer@example.test'}})}),jar.jar,'checkout');
  expect(response.status).toBe(409);expect(response.headers.get('content-type')).toContain('application/problem+json');
  const text=await response.text();expect(JSON.parse(text).code).toBe('inventory_insufficient');expect(text).not.toContain('private');
});
test('cookie storage failures return an unavailable context instead of breaking SSR',async()=>{
  const jar={get(){throw new Error('cookie storage blocked');},set(){throw new Error('cookie storage blocked');}} as unknown as AstroCookies;
  expect((await readPurchaseContext(request('context'),jar)).enabled).toBe(false);
});
