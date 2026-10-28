/** Responsibility: real customer BFF transport gates; dependencies: generated HTTP client and Astro cookie seam.
 * Callers: CI Bun suite; invariants: backend identity, cookie custody, exact CSRF, no guest downgrade or account oracle.
 */
import { afterEach, beforeEach, expect, spyOn, test } from 'bun:test';
import { createHmac, randomBytes } from 'node:crypto';
import type { AstroCookies } from 'astro';
import { handlePurchaseRequest, readPurchaseContext } from './commerce-server';
const site='https://shop.example.test', origin='http://api:8080', tenant='c0900001-0000-4000-8000-000000000001';
const customer='c09f0001-0000-4000-8000-000000000001', cart='c09f0002-0000-4000-8000-000000000001';
const sku='c0900008-0000-4000-8000-000000000001', sessionName='__Host-osc_customer_session';
const bearer=randomBytes(32).toString('base64url'), challenge='1234567890.'+randomBytes(32).toString('base64url')+'.'+randomBytes(32).toString('base64url');
const password=randomBytes(24).toString('base64url');
const names=['OSC_COMMERCE_MODE','OSC_COMMERCE_API_ORIGIN','OSC_COMMERCE_SITE','OSC_COMMERCE_COOKIE_KEY','OSC_STOREFRONT_ALLOWED_HOSTS','OPENSITE_STOREFRONT_ORIGIN','OPENSITE_CUSTOMER_CSRF_SECRET'];
const saved=new Map(names.map(name=>[name,process.env[name]]));
const calls:Request[]=[];
let mock:ReturnType<typeof spyOn>, denied=0, foreign=false;
const values=new Map<string,string>(), flags=new Map<string,Record<string,unknown>>();
const jar={get:(name:string)=>values.has(name)?{value:values.get(name)!}:undefined,set:(name:string,value:string,options:Record<string,unknown>)=>{if(options.maxAge===0)values.delete(name);else values.set(name,value);flags.set(name,options);}} as unknown as AstroCookies;
function csrf() { return createHmac('sha256',Buffer.from(process.env.OPENSITE_CUSTOMER_CSRF_SECRET!,'base64url')).update(`customer-http:${site}:customer-session:${tenant}:${bearer}`).digest('base64url'); }
function session() { return {customer:{tenant_id:foreign?'c0900001-0000-4000-8000-000000000002':tenant,customer_id:customer,session_id:cart,email:'buyer@example.test',expires_at:'2000-01-01T00:00:00Z',idle_expires_at:'2000-01-01T00:00:00Z'},csrf_token:csrf()}; }
function request(operation:string,token?:string,input?:unknown,method=input?'POST':'GET') {
  return new Request(`${site}/purchase/api/${operation}`,{method,headers:{Host:'shop.example.test',...(token?{Origin:site,'X-CSRF-Token':token,'Content-Type':'application/json'}:{}),'Idempotency-Key':'c09f0001-0000-4000-8000-000000000001'},...(input?{body:JSON.stringify(input)}:{})});
}
async function send(operation:string,token?:string,input?:unknown,method?:string) { return handlePurchaseRequest(request(operation,token,input,method),jar,operation); }
beforeEach(()=>{
  Object.assign(process.env,{OSC_COMMERCE_MODE:'backend',OSC_COMMERCE_API_ORIGIN:origin,OSC_COMMERCE_SITE:site,OSC_COMMERCE_COOKIE_KEY:randomBytes(32).toString('base64url'),OSC_STOREFRONT_ALLOWED_HOSTS:'shop.example.test',OPENSITE_STOREFRONT_ORIGIN:site,OPENSITE_CUSTOMER_CSRF_SECRET:randomBytes(32).toString('base64url')});
  calls.length=0;values.clear();flags.clear();denied=0;foreign=false;
  mock=spyOn(globalThis,'fetch').mockImplementation(Object.assign(async(input:RequestInfo|URL,init?:RequestInit)=>{
    const r=input instanceof Request?input:new Request(input,init);calls.push(r);const path=new URL(r.url).pathname;
    if(path==='/api/v1/public/sites/current')return Response.json({tenant_id:tenant,products:[{id:sku,name:'Tote',slug:'tote',skus:[{sku_id:sku,sku_code:'TOTE',option_values:[],price_minor:'2400',currency:'USD',available:true}]}]});
    if(path.includes('/customer/')) {
      if(denied&&(r.method!=='GET'||values.has(sessionName)))return Response.json({code:'private_account_exists',detail:'secret internals'},{status:denied});
      const cookie=r.headers.get('Cookie')??'';
      if(path.endsWith('/session')&&r.method==='GET') {
        if(cookie.includes(sessionName+'='))return Response.json(session());
        return Response.json({customer:null,csrf_token:challenge},{headers:{'Set-Cookie':`__Host-osc_customer_challenge=${challenge}; Path=/; Secure; HttpOnly; SameSite=Lax; Expires=Wed, 01 Jan 2098 00:00:00 GMT`}});
      }
      if(path.endsWith('/session')&&r.method==='POST'){
        const headers=new Headers();headers.append('Set-Cookie',`${sessionName}=${bearer}; Path=/; Secure; HttpOnly; SameSite=Lax; Expires=Wed, 01 Jan 2098 00:00:00 GMT`);
        headers.append('Set-Cookie','__Host-osc_customer_challenge=; Path=/; Secure; HttpOnly; SameSite=Lax; Max-Age=0');
        return Response.json(session(),{headers});
      }
      if(path.endsWith('/session')&&r.method==='DELETE')return new Response(null,{status:204,headers:{'Set-Cookie':`${sessionName}=; Path=/; Secure; HttpOnly; SameSite=Lax; Max-Age=0`}});
      if(path.endsWith('/orders'))return Response.json({items:[{id:cart,order_number:9007199254740991,state:'paid',currency:'USD',total_minor:'3140',created_at:'2026-10-23T00:00:00Z'}]});
      if(path.endsWith('/orders/'+cart))return Response.json({code:'not_found'},{status:404});
      if(path.endsWith('/verifications'))return new Response(null,{status:204});
      if(path.endsWith('/registrations'))return Response.json({status:'verification_pending'},{status:202});
    }
    const rawCart={id:cart,currency:'USD',version:1,lines:[],subtotal:0},headers={ETag:`"v1-${'a'.repeat(64)}"`};
    if(path.endsWith('/carts'))return Response.json({cart:rawCart},{status:201,headers});
    if(path.includes('/carts/'))return Response.json(rawCart,{headers});
    if(path.endsWith('/checkout/preview'))return Response.json({subtotal_minor:2400,tax_minor:240,shipping_minor:500,total_minor:3140,currency:'USD',shipping_options:[]});
    return new Response(null,{status:404});
  },{preconnect:globalThis.fetch.preconnect}));
});
afterEach(()=>{mock?.mockRestore();for(const name of names){const value=saved.get(name);if(value===undefined)delete process.env[name];else process.env[name]=value;}});
test('registration verification login session and logout use real generated routes with server-only custody',async()=>{
  let context=await readPurchaseContext(request('context'),jar);
  expect(context.enabled).toBe(true);
  expect((await send('customer/register',context.csrf,{email:'buyer@example.test',password})).status).toBe(202);
  expect((await send('customer/verify',context.csrf,{token:bearer})).status).toBe(200);
  const login=await send('customer/session',context.csrf,{email:'buyer@example.test',password});
  expect(login.status).toBe(200);expect(await login.text()).not.toContain(bearer);
  expect(values.get(sessionName)).toBe(bearer);
  expect(values.has('__Host-osc_customer_challenge')).toBe(false);
  expect(flags.get(sessionName)).toMatchObject({secure:true,httpOnly:true,path:'/',sameSite:'lax'});
  context=await readPurchaseContext(request('context'),jar);expect(context.actor).toBe('customer');
  expect(await (await send('customer/session')).json()).toMatchObject({actor:'customer'});
  expect((await send('customer/session',context.csrf,undefined,'DELETE')).status).toBe(200);
  expect((await readPurchaseContext(request('context'),jar)).actor).toBe('guest');
  const register=calls.find(r=>r.url.endsWith('/registrations'))!;
  expect(register.headers.get('Origin')).toBe(site);expect(register.headers.get('X-CSRF-Token')).toBe(challenge);
  expect(register.headers.get('Cookie')).toContain('__Host-osc_customer_challenge='+challenge);
  expect(calls.every(r=>!r.url.includes('__fixture')&&!r.url.includes(bearer))).toBe(true);
});
test('customer cart and checkout carry validated customer cookie and CSRF without guest token',async()=>{
  values.set(sessionName,bearer);
  const context=await readPurchaseContext(request('context'),jar);expect(context.actor).toBe('customer');
  expect((await send('cart',context.csrf,{sku_id:sku,quantity:1})).status).toBe(200);
  expect((await send('preview',context.csrf,{shipping_address:{country:'US',postal_code:'10001'}})).status).toBe(200);
  for(const r of calls.filter(r=>/\/(carts|checkout)/.test(r.url))) {
    expect(r.headers.get('Cookie')).toBe(`${sessionName}=${bearer}`);
    expect(r.headers.has('X-Cart-Token')).toBe(false);
    if(r.method!=='GET'){expect(r.headers.get('Origin')).toBe(site);expect(r.headers.get('X-CSRF-Token')).toBe(csrf());}
  }
});
for(const status of [401,503])test(`customer read failure ${status} never downgrades a purchase to guest`,async()=>{
  const context=await readPurchaseContext(request('context'),jar);values.set(sessionName,bearer);denied=status;
  expect((await send('cart',context.csrf,{sku_id:sku,quantity:1})).status).toBe(status);
  expect(calls.some(r=>r.url.endsWith('/carts'))).toBe(false);
  if(status===503)expect(values.get(sessionName)).toBe(bearer);
  expect((await readPurchaseContext(request('context'),jar)).enabled).toBe(false);
});
test('foreign tenant identity and malformed bearer fail closed',async()=>{
  values.set(sessionName,bearer);foreign=true;
  expect((await readPurchaseContext(request('context'),jar)).enabled).toBe(false);
  foreign=false;values.set(sessionName,'forged');
  expect((await readPurchaseContext(request('context'),jar)).enabled).toBe(false);
});
test('customer writes reject foreign Origin and forged browser CSRF before upstream IO',async()=>{
  const context=await readPurchaseContext(request('context'),jar), before=calls.length;
  for(const [origin,token] of [['https://evil.example',context.csrf],[site,'forged']]) {
    const r=request('customer/register',token,{email:'buyer@example.test',password});r.headers.set('Origin',origin!);
    expect((await handlePurchaseRequest(r,jar,'customer/register')).status).toBe(403);
  }
  expect(calls.length).toBe(before);
});
test('duplicate customer cookies cannot choose an identity',async()=>{
  const context=await readPurchaseContext(request('context'),jar),before=calls.length;
  const r=request('cart',context.csrf,{sku_id:sku,quantity:1});r.headers.set('Cookie',`${sessionName}=${bearer}; ${sessionName}=forged`);
  expect((await handlePurchaseRequest(r,jar,'cart')).status).toBe(401);expect(calls.length).toBe(before);
});
test('logout discards customer cart custody even if the browser replays its old signed BFF cookie',async()=>{
  values.set(sessionName,bearer);let context=await readPurchaseContext(request('context'),jar);
  expect((await send('cart',context.csrf,{sku_id:sku,quantity:1})).status).toBe(200);
  const old=values.get('__Host-osc_storefront_cart')!;
  expect(Buffer.from(old.split('.')[0]!,'base64url').toString()).not.toContain(bearer);
  context=await readPurchaseContext(request('context'),jar);
  expect((await send('customer/session',context.csrf,undefined,'DELETE')).status).toBe(200);
  values.set('__Host-osc_storefront_cart',old);const before=calls.length;
  expect(await (await send('cart')).json()).toBeNull();
  expect(calls.slice(before).some(r=>r.url.includes('/carts/'))).toBe(false);
});
test('login and registration failures do not relay account-existence codes or private detail',async()=>{
  const context=await readPurchaseContext(request('context'),jar);
  for(const operation of ['customer/register','customer/session'])for(const status of [400,401,409,422]) {
    denied=status;const response=await send(operation,context.csrf,{email:'buyer@example.test',password});
    expect(response.status).toBe(400);expect(await response.json()).toMatchObject({code:'customer_auth_failed'});
  }
});

test('history forwards only the validated customer cookie and preserves exact money and foreign 404',async()=>{
  values.set(sessionName,bearer);await readPurchaseContext(request('context'),jar);
  const list=await send('history');expect(list.status).toBe(200);
  expect(list.headers.get('Cache-Control')).toBe('no-store');
  expect(await list.json()).toMatchObject({items:[{id:cart,order_number:'9007199254740991',total_minor:'3140'}]});
  expect((await send('history/'+cart)).status).toBe(404);
  for(const r of calls.filter(r=>r.url.includes('/customer/orders'))){
    expect(r.headers.get('Cookie')).toBe(`${sessionName}=${bearer}`);
    expect(r.headers.has('Authorization')).toBe(false);expect(r.headers.has('X-Cart-Token')).toBe(false);
  }
  values.delete(sessionName);const before=calls.length;
  expect((await send('history')).status).toBe(401);
  expect(calls.slice(before).some(r=>r.url.includes('/customer/orders'))).toBe(false);
});

for(const [label,fields,authenticated,status] of [
  ['empty',{},false,200],
  ['guest',{c:cart,t:bearer},false,200],
  ['customer',{c:cart,u:customer},true,200],
  ['cart without authority',{c:cart},false,401],
  ['token without cart',{t:bearer},false,401],
] as const)test(`signed purchasing cookie matrix: ${label}`,async()=>{
  if(authenticated)values.set(sessionName,bearer);
  const encoded=Buffer.from(JSON.stringify({s:bearer,x:Date.now()+60_000,tenant,...fields})).toString('base64url');
  const signature=createHmac('sha256',Buffer.from(process.env.OSC_COMMERCE_COOKIE_KEY!,'base64url')).update(encoded).digest('base64url');
  values.set('__Host-osc_storefront_cart',`${encoded}.${signature}`);
  expect((await send('cart')).status).toBe(status);
  if(status===401)expect(calls).toHaveLength(0);
});
for(const [operation,method,input] of [
  ['customer/session','DELETE',undefined],
  ['customer/register','POST',{email:'buyer@example.test',password}],
  ['customer/verify','POST',{token:bearer}],
] as const)test(`one session validation per authenticated account write: ${operation} ${method}`,async()=>{
  values.set(sessionName,bearer);
  const context=await readPurchaseContext(request('context'),jar);calls.length=0;
  expect((await send(operation,context.csrf,input,method)).ok).toBe(true);
  expect(calls.filter(r=>r.method==='GET'&&r.url.endsWith('/customer/session'))).toHaveLength(1);
});
test('account login revalidates changed identity and later requests never reuse old validation',async()=>{
  const context=await readPurchaseContext(request('context'),jar);calls.length=0;
  expect((await send('customer/session',context.csrf,{email:'buyer@example.test',password})).status).toBe(200);
  expect(calls.filter(r=>r.method==='GET'&&r.url.endsWith('/customer/session'))).toHaveLength(2);
  denied=401;
  const response=await send('customer/verify',context.csrf,{token:bearer});
  expect(response.status).toBe(403); // login rotates browser CSRF
  expect((await send('history')).status).toBe(401);
});
