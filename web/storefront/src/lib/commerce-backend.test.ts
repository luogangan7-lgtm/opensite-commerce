/** Responsibility: real-backend purchasing admission and lossless transport regressions.
 * Dependencies: public generated API contracts and Astro cookie seam; caller: CI Bun suite.
 * Invariants: no fixture dependency, cookie-only capability, exact Origin/CSRF, safe problem codes.
 */
import { afterEach, beforeEach, expect, spyOn, test } from 'bun:test';
import { createHmac, randomBytes } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { createServer as httpServer, get as httpGet, type RequestListener, type Server } from 'node:http';
import { createServer as httpsServer, get as httpsGet } from 'node:https';
import type { AddressInfo } from 'node:net';
import type { AstroCookies } from 'astro';
import { handlePurchaseRequest, readPurchaseContext } from './commerce-server';
const site = 'https://shop.example.test', tenant = 'c0900001-0000-4000-8000-000000000001';
const sku = 'c0900008-0000-4000-8000-000000000001', product = 'c0900007-0000-4000-8000-000000000001';
const cart = 'c09d0001-0000-4000-8000-000000000001', token = randomBytes(32).toString('base64url');
const etag = `"v1-${'a'.repeat(64)}"`;
const names = ['OSC_COMMERCE_MODE','OSC_COMMERCE_API_ORIGIN','OSC_COMMERCE_SITE','OSC_COMMERCE_COOKIE_KEY','OSC_STOREFRONT_ALLOWED_HOSTS','OPENSITE_STOREFRONT_ORIGIN','OPENSITE_STOREFRONT_KEY','OSC_COMMERCE_TEST_HTTP','OPENSITE_ENVIRONMENT','GITHUB_ACTIONS'];
const saved = new Map(names.map(name => [name, process.env[name]]));
let mock: ReturnType<typeof spyOn>;
const calls: Request[] = [];
let failure = false, failureOnlyCheckout = false, failureCode='stock_unavailable';
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
  (globalThis as typeof globalThis & {__OSC_COMMERCE_TEST_HTTP__?:boolean}).__OSC_COMMERCE_TEST_HTTP__=true;
  Object.assign(process.env, {OSC_COMMERCE_MODE:'backend',OSC_COMMERCE_API_ORIGIN:'http://api:8080',OSC_COMMERCE_SITE:site,OSC_COMMERCE_COOKIE_KEY:randomBytes(32).toString('base64url'),OSC_STOREFRONT_ALLOWED_HOSTS:'shop.example.test',OPENSITE_STOREFRONT_ORIGIN:site,OPENSITE_STOREFRONT_KEY:randomBytes(32).toString('base64url')});
  calls.length=0; failure=false;failureOnlyCheckout=false;failureCode='stock_unavailable';
  mock=spyOn(globalThis,'fetch').mockImplementation(Object.assign(async(input:RequestInfo|URL,init?:RequestInit)=>{
    const req=input instanceof Request ? input : new Request(input,init); calls.push(req);
    if(new URL(req.url).pathname==='/api/v1/public/sites/current') return Response.json({tenant_id:tenant,hostname:'shop.example.test',locale:'en',products:[{id:product,name:'Canvas tote',slug:'canvas-tote',skus:[{sku_id:sku,sku_code:'TOTE',option_values:[],price_minor:'2400',currency:'USD',available:true}]}]});
    if(failure && (!failureOnlyCheckout || req.url.endsWith('/checkout'))) return Response.json({type:'about:blank',status:409,code:failureCode,detail:'postgres://private/internal'}, {status:409,headers:{'Content-Type':'application/problem+json'}});
    if(req.url.endsWith('/carts')) return Response.json({cart:{id:cart,currency:'USD',version:1,lines:[],subtotal:0},cart_token:token},{status:201,headers:{ETag:etag}});
    return new Response(`{"id":"${cart}","currency":"USD","version":1,"lines":[{"sku_id":"${sku}","quantity":1,"sellable":true,"unit_amount":9223372036854775807,"line_amount":9223372036854775807}],"subtotal":9223372036854775807}`,{headers:{ETag:etag,'Content-Type':'application/json'}});
  },{preconnect:globalThis.fetch.preconnect}));
});
afterEach(()=>{delete (globalThis as typeof globalThis & {__OSC_COMMERCE_TEST_HTTP__?:boolean}).__OSC_COMMERCE_TEST_HTTP__;mock?.mockRestore();for(const name of names){const value=saved.get(name);if(value===undefined)delete process.env[name];else process.env[name]=value;}});
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
  const response=await handlePurchaseRequest(new Request(`${site}/purchase/api/checkout`,{method:'POST',headers:{Host:'shop.example.test',Origin:site,'X-CSRF-Token':context.csrf,'Content-Type':'application/json','Idempotency-Key':'c09d0001-0000-4000-8000-000000000001'},body:JSON.stringify({shipping_address:{country:'US',address1:'1 Test Way',city:'New York',postal_code:'10001'},buyer_contact:{email:'buyer@example.test'},accepted_total:{amount_minor:'160',currency:'USD'}})}),jar.jar,'checkout');
  expect(response.status).toBe(409);expect(response.headers.get('content-type')).toContain('application/problem+json');
  const text=await response.text();expect(JSON.parse(text).code).toBe('stock_unavailable');expect(text).not.toContain('private');
  expect(calls.at(-1)!.headers.get('Idempotency-Key')).toBe('c09d0001-0000-4000-8000-000000000001');
});
test('checkout accepts uppercase UUIDs and forwards lowercase',async()=>{
  const jar=cookies(),context=await readPurchaseContext(request('context'),jar.jar);
  await handlePurchaseRequest(request('cart',context.csrf,{sku_id:sku,quantity:1}),jar.jar,'cart');
  failure=true;failureOnlyCheckout=true;
  const key='abcdef01-2345-6789-abcd-ef0123456789';
  const response=await handlePurchaseRequest(new Request(`${site}/purchase/api/checkout`,{method:'POST',headers:{Host:'shop.example.test',Origin:site,'X-CSRF-Token':context.csrf,'Content-Type':'application/json','Idempotency-Key':key.toUpperCase()},body:JSON.stringify({shipping_address:{country:'US',postal_code:'10001'},accepted_total:{amount_minor:'2400',currency:'USD'}})}),jar.jar,'checkout');
  expect(response.status).toBe(409);
  expect(calls.at(-1)!.headers.get('Idempotency-Key')).toBe(key);
});
test('checkout rejects missing, noncanonical and repeated idempotency keys before forwarding a write',async()=>{
  const jar=cookies(),context=await readPurchaseContext(request('context'),jar.jar);
  await handlePurchaseRequest(request('cart',context.csrf,{sku_id:sku,quantity:1}),jar.jar,'cart');
  const canonical='abcdef01-2345-6789-abcd-ef0123456789';
  const before=calls.filter(call=>call.method!=='GET').length;
  for(const key of [undefined,'','alice@example.test','legacy-key',canonical.replaceAll('-',''),`{${canonical}}`,`${canonical}.reserve`,`${canonical.slice(0,-1)}g`,`${canonical}, ${canonical}`]){
    const headers=new Headers({Host:'shop.example.test',Origin:site,'X-CSRF-Token':context.csrf,'Content-Type':'application/json'});
    if(key!==undefined)headers.set('Idempotency-Key',key);
    const response=await handlePurchaseRequest(new Request(`${site}/purchase/api/checkout`,{method:'POST',headers,body:'{}'}),jar.jar,'checkout');
    expect(response.status).toBe(400);expect(response.headers.get('Content-Type')).toBe('application/problem+json');
    expect(await response.json()).toEqual({type:'about:blank',status:400,code:'invalid_idempotency_key'});
  }
  expect(calls.filter(call=>call.method!=='GET')).toHaveLength(before);
});
test('cookie storage failures return an unavailable context instead of breaking SSR',async()=>{
  const jar={get(){throw new Error('cookie storage blocked');},set(){throw new Error('cookie storage blocked');}} as unknown as AstroCookies;
  expect((await readPurchaseContext(request('context'),jar)).enabled).toBe(false);
});
test('backend writes reject foreign Origin and altered CSRF before any upstream IO',async()=>{
  const jar=cookies(), context=await readPurchaseContext(request('context'),jar.jar);
  for (const headers of [{Origin:'https://foreign.example.test','X-CSRF-Token':context.csrf}, {Origin:site,'X-CSRF-Token':`${context.csrf}x`}]) {
    const count=calls.length;
    const response=await handlePurchaseRequest(new Request(`${site}/purchase/api/cart`,{method:'POST',headers:{Host:'shop.example.test','Content-Type':'application/json',...headers},body:JSON.stringify({sku_id:sku,quantity:1})}),jar.jar,'cart');
    expect(response.status).toBe(403);expect(calls.length).toBe(count);
  }
});
test('signed cookie tampering and forwarded host cannot reach the cart capability',async()=>{
  const jar=cookies(),context=await readPurchaseContext(request('context'),jar.jar);
  await handlePurchaseRequest(request('cart',context.csrf,{sku_id:sku,quantity:1}),jar.jar,'cart');
  const name='__Host-osc_storefront_cart', value=jar.values.get(name)!;
  jar.values.set(name,value+'x');const count=calls.length;
  expect((await handlePurchaseRequest(request('cart'),jar.jar,'cart')).status).toBe(401);
  expect(calls.length).toBe(count);
  const forwarded=request('context');forwarded.headers.set('X-Forwarded-Host','shop.example.test');
  expect((await readPurchaseContext(forwarded,jar.jar)).enabled).toBe(false);expect(calls.length).toBe(count);
});
test('checkout sends a server proof bound to the cart token while exposing only problem codes',async()=>{
  const jar=cookies(),context=await readPurchaseContext(request('context'),jar.jar);
  await handlePurchaseRequest(request('cart',context.csrf,{sku_id:sku,quantity:1}),jar.jar,'cart');
  failure=true;
  const response=await handlePurchaseRequest(request('preview',context.csrf,{shipping_address:{country:'US',postal_code:'10001'}}),jar.jar,'preview');
  expect(response.status).toBe(409);
  const outbound=calls.at(-1)!;
  const proof=createHmac('sha256',Buffer.from(process.env.OPENSITE_STOREFRONT_KEY!,'base64url')).update(`checkout:${tenant}:${token}:${site}`).digest('base64url');
  expect(outbound.headers.get('X-CSRF-Token')).toBe(proof);expect(outbound.headers.get('X-Cart-ID')).toBe(cart);
  expect(outbound.headers.get('X-Cart-Token')).toBe(token);expect(outbound.url).not.toContain(token);
  const text=await response.text();expect(text).not.toContain(proof);expect(text).not.toContain(token);
});

test('unlisted format-valid upstream codes are redacted',async()=>{
  const jar=cookies(),context=await readPurchaseContext(request('context'),jar.jar);
  await handlePurchaseRequest(request('cart',context.csrf,{sku_id:sku,quantity:1}),jar.jar,'cart');
  failure=true;failureCode='private_customer_lookup_failed';
  const response=await handlePurchaseRequest(request('preview',context.csrf,{shipping_address:{country:'US',postal_code:'10001'}}),jar.jar,'preview');
  expect(response.status).toBe(409);expect(await response.json()).toMatchObject({code:'purchase_request_failed'});
});

test('GITHUB_ACTIONS cannot relax HTTPS; HTTP needs explicit test environment and flag',async()=>{
  const jar=cookies(),loopback='http://127.0.0.1:4321';
  Object.assign(process.env,{OSC_COMMERCE_SITE:loopback,OSC_STOREFRONT_ALLOWED_HOSTS:'127.0.0.1',GITHUB_ACTIONS:'true',OPENSITE_ENVIRONMENT:'test'});
  delete process.env.OSC_COMMERCE_TEST_HTTP;
  const req=()=>new Request(loopback,{headers:{Host:'127.0.0.1:4321'}});
  expect((await readPurchaseContext(req(),jar.jar)).enabled).toBe(false);
  process.env.OSC_COMMERCE_TEST_HTTP='1';
  expect((await readPurchaseContext(req(),jar.jar)).enabled).toBe(true);
  process.env.OPENSITE_ENVIRONMENT='production';
  expect((await readPurchaseContext(req(),jar.jar)).enabled).toBe(false);
});

test('missing build define forbids HTTP before upstream IO',async()=>{
  delete (globalThis as typeof globalThis & {__OSC_COMMERCE_TEST_HTTP__?:boolean}).__OSC_COMMERCE_TEST_HTTP__;
  Object.assign(process.env,{OSC_COMMERCE_SITE:'http://127.0.0.1:4321',OSC_STOREFRONT_ALLOWED_HOSTS:'127.0.0.1',OSC_COMMERCE_TEST_HTTP:'1',OPENSITE_ENVIRONMENT:'test'});
  const count=calls.length;
  expect((await readPurchaseContext(new Request('http://127.0.0.1:4321',{headers:{Host:'127.0.0.1:4321'}}),cookies().jar)).enabled).toBe(false);
  expect(calls.length).toBe(count);
});
// Execute the adapter's public production entry; source imports above cannot
// witness define replacement, route loading or the built Node request adapter.
test('production entry rejects HTTP with zero upstream IO and accepts HTTPS',async()=>{
  const autostart=process.env.ASTRO_NODE_AUTOSTART;
  process.env.ASTRO_NODE_AUTOSTART='disabled';
  const directory=mkdtempSync(join(tmpdir(),'b-payret-tls-'));
  const servers: Server[]=[];
  const listen=async(server:Server)=>{
    servers.push(server);
    await new Promise<void>((resolve,reject)=>{server.once('error',reject);server.listen(0,'127.0.0.1',resolve);});
    return (server.address() as AddressInfo).port;
  };
  const get=async(url:string)=>new Promise<{status:number;body:string}>((resolve,reject)=>{
    const receive=(response:import('node:http').IncomingMessage)=>{
      let body='';response.setEncoding('utf8');response.on('data',chunk=>{body+=chunk;});
      response.on('end',()=>resolve({status:response.statusCode!,body}));response.on('error',reject);
    };
    const req=url.startsWith('https:')?httpsGet(url,{rejectUnauthorized:false},receive):httpGet(url,receive);
    req.setTimeout(5000,()=>req.destroy(new Error('production entry response timeout')));req.on('error',reject);
  });
  try {
    const certificate=spawnSync('openssl',['req','-x509','-newkey','rsa:2048','-nodes','-keyout',join(directory,'key.pem'),'-out',join(directory,'cert.pem'),'-days','1','-subj','/CN=127.0.0.1'],{timeout:10000,stdio:'pipe'});
    expect(certificate.status).toBe(0);
    const entry=await import(new URL('../../dist/server/entry.mjs',import.meta.url).href);
    expect(typeof entry.handler).toBe('function');
    const handler=entry.handler as RequestListener;
    const plain=await listen(httpServer(handler));
    const secure=await listen(httpsServer({key:readFileSync(join(directory,'key.pem')),cert:readFileSync(join(directory,'cert.pem'))},handler));
    Object.assign(process.env,{OSC_COMMERCE_TEST_HTTP:'1',OPENSITE_ENVIRONMENT:'test',OSC_STOREFRONT_ALLOWED_HOSTS:'127.0.0.1'});
    // A hostile runtime test flag/global must not re-enable a production build.
    for(const protocol of ['http','https']) {
      const origin=`${protocol}://127.0.0.1:${protocol==='http'?plain:secure}`;
      process.env.OSC_COMMERCE_SITE=origin;
      const count=calls.length;
      const response=await get(`${origin}/purchase/api/context`);
      expect(response.status).toBe(200);
      expect(JSON.parse(response.body).enabled).toBe(protocol==='https');
      expect(calls.length-count).toBe(protocol==='https'?1:0);
    }
  } finally {
    await Promise.all(servers.map(server=>new Promise<void>((resolve,reject)=>{server.close(error=>error?reject(error):resolve());server.closeAllConnections();})));
    rmSync(directory,{recursive:true,force:true});
    if(autostart===undefined)delete process.env.ASTRO_NODE_AUTOSTART;else process.env.ASTRO_NODE_AUTOSTART=autostart;
  }
},20000);
