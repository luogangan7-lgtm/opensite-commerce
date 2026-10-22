/** Responsibility: BFF payment protocol witnesses in backend and synthetic modes.
 * Dependencies: generated HTTP boundary via fetch seam; caller: CI Bun suite.
 * Invariants: exact signed bytes/account, receipt is not CheckoutResult, persisted state owns completion.
 * Actual DB freshness/account/replay effects are covered by TestPaymentsPG* in foundation.
 */
import { afterEach, beforeEach, expect, spyOn, test } from 'bun:test';
import { createHmac, randomBytes } from 'node:crypto';
import type { AstroCookies } from 'astro';
import { handlePurchaseRequest, readPurchaseContext } from './commerce-server';
const tenant='c0900001-0000-4000-8000-000000000001', sku='c09c0001-0000-4000-8000-000000000001';
const cart='c09c0003-0000-4000-8000-000000000001', checkoutID='c09c0010-0000-4000-8000-000000000001';
const order='c09c0011-0000-4000-8000-000000000001', token=randomBytes(32).toString('base64url');
const names=['OSC_COMMERCE_MODE','OSC_COMMERCE_SITE','OSC_COMMERCE_API_ORIGIN','OSC_COMMERCE_COOKIE_KEY','OSC_STOREFRONT_ALLOWED_HOSTS','OPENSITE_STOREFRONT_ORIGIN','OPENSITE_STOREFRONT_KEY','OPENSITE_FAKE_PSP','OPENSITE_ENVIRONMENT','OPENSITE_FAKE_WEBHOOK_KEY','OPENSITE_FAKE_ACCOUNT_ID','OSC_COMMERCE_FIXTURE_MODE','OSC_COMMERCE_FIXTURE_SITE','OSC_COMMERCE_FIXTURE_ORIGIN'];
const saved=new Map(names.map(name=>[name,process.env[name]]));
let mock: ReturnType<typeof spyOn>;
beforeEach(()=>Object.assign(process.env,{OSC_COMMERCE_MODE:'backend',OSC_COMMERCE_SITE:'https://shop.example.test',OSC_COMMERCE_API_ORIGIN:'http://api:8080',OSC_COMMERCE_COOKIE_KEY:randomBytes(32).toString('base64url'),OSC_STOREFRONT_ALLOWED_HOSTS:'shop.example.test',OPENSITE_STOREFRONT_ORIGIN:'https://shop.example.test',OPENSITE_STOREFRONT_KEY:randomBytes(32).toString('base64url'),OPENSITE_FAKE_PSP:'1',OPENSITE_ENVIRONMENT:'test',OPENSITE_FAKE_WEBHOOK_KEY:randomBytes(32).toString('base64url'),OPENSITE_FAKE_ACCOUNT_ID:'acct_bff_test'}));
afterEach(()=>{mock?.mockRestore();for(const name of names){const value=saved.get(name);if(value===undefined)delete process.env[name];else process.env[name]=value;}});
async function setup(fixture=false) {
  const site=fixture?'http://127.0.0.1:4323':'https://shop.example.test';
  if(fixture){delete process.env.OSC_COMMERCE_MODE;Object.assign(process.env,{OSC_COMMERCE_FIXTURE_MODE:'contract-test',OSC_COMMERCE_FIXTURE_SITE:site,OSC_COMMERCE_FIXTURE_ORIGIN:'http://127.0.0.1:4176'});}
  const values=new Map<string,string>();
  const jar={get:(name:string)=>values.has(name)?{value:values.get(name)!}:undefined,set:(name:string,value:string)=>{values.set(name,value);}} as unknown as AstroCookies;
  const calls:Request[]=[], events:{raw:string;signature:string}[]=[];
  const result={checkout_id:checkoutID,order_id:order,state:'pending_payment',amount_minor:'9223372036854775807',currency:'USD',order_version:'1',payment:{provider_ref:'fake_bff',status:'authorized'}};
  let receiptStatus='processed', receiptID:string|undefined, rejection=0, malformed=false, applied=false;
  mock=spyOn(globalThis,'fetch').mockImplementation(Object.assign(async(input:RequestInfo|URL,init?:RequestInit)=>{
    const req=input instanceof Request?input:new Request(input,init);calls.push(req);
    const path=new URL(req.url).pathname;
    const product={id:'c0900007-0000-4000-8000-000000000001',product_id:'c0900007-0000-4000-8000-000000000001',name:'Tote',slug:'tote',skus:[{sku_id:sku,sku_code:'TOTE',option_values:[],price_minor:'2400',currency:'USD',available:true}]};
    if(path==='/api/v1/public/sites/current'||path==='/__fixture/context')return Response.json({tenant_id:tenant,actor:'guest',products:[product]});
    const rawCart={id:cart,currency:'USD',version:1,lines:[],subtotal:0},headers={ETag:`"v1-${'a'.repeat(64)}"`};
    if(path.endsWith('/carts'))return Response.json({cart:rawCart,cart_token:token},{status:201,headers});
    if(path.includes('/carts/'))return Response.json(rawCart,{headers});
    if(path.endsWith('/webhook')||path==='/__fixture/payment'){
      const raw=await req.text();events.push({raw,signature:req.headers.get('X-Fake-Signature')??''});
      if(rejection)return Response.json({code:'unauthenticated',detail:'private signing material'},{status:rejection});
      if(applied){result.state='paid';result.payment.status='captured';}
      return Response.json(malformed?{...result}: {event_id:receiptID??(fixture?'fixture-event':JSON.parse(raw).event_id),status:receiptStatus});
    }
    if(path.endsWith('/checkout')||path.includes('/checkout/')||path==='/__fixture/completion')return Response.json(result);
    return new Response(null,{status:404});
  },{preconnect:globalThis.fetch.preconnect}));
  const req=(operation:string,body?:unknown)=>new Request(`${site}/purchase/api/${operation}`,{method:body?'POST':'GET',headers:{Host:new URL(site).host,...(body?{Origin:site,'Content-Type':'application/json','X-CSRF-Token':context.csrf,'Idempotency-Key':'same-start-key'}:{})},...(body?{body:JSON.stringify(body)}:{})});
  const context=await readPurchaseContext(new Request(site,{headers:{Host:new URL(site).host}}),jar);
  expect(context.enabled).toBe(true);
  expect((await handlePurchaseRequest(req('cart',{sku_id:sku,quantity:1}),jar,'cart')).status).toBe(200);
  expect((await handlePurchaseRequest(req('checkout',{shipping_address:{country:'US',address1:'1 Test',city:'City',postal_code:'10001'},buyer_contact:{email:'buyer@example.test'},accepted_total:{amount_minor:result.amount_minor,currency:result.currency}}),jar,'checkout')).status).toBe(201);
  return {calls,events,result,context,send:(outcome='captured',extra={})=>handlePurchaseRequest(req('payment',{checkout_id:checkoutID,outcome,accepted_total:{amount_minor:result.amount_minor,currency:result.currency},...extra}),jar,'payment'),completion:()=>handlePurchaseRequest(req('completion'),jar,'completion'),pending:()=>{receiptStatus='pending';},receipt:(status:string,id?:string)=>{receiptStatus=status;receiptID=id;},reject:(status:number)=>{rejection=status;},malformed:()=>{malformed=true;},apply:()=>{applied=true;}};
}
test('BFF signs timestamp and account-bearing exact bytes with captured, never tenant or authorized',async()=>{
  const f=await setup();const before=Math.floor(Date.now()/1000);
  expect((await f.send()).status).toBe(200);expect(f.events).toHaveLength(1);
  const {raw,signature}=f.events[0]!,parts=/^t=([0-9]{1,12}),v1=([a-f0-9]{64})$/.exec(signature);
  expect(parts).not.toBeNull();expect(Number(parts![1])).toBeGreaterThanOrEqual(before);
  expect(Number(parts![1])).toBeLessThanOrEqual(Math.floor(Date.now()/1000));
  expect(parts![2]).toBe(createHmac('sha256',Buffer.from(process.env.OPENSITE_FAKE_WEBHOOK_KEY!,'base64url')).update(`${parts![1]}.${raw}`).digest('hex'));
  expect(JSON.parse(raw)).toMatchObject({account_id:'acct_bff_test',status:'captured',order_id:order});
  expect(raw).not.toContain('tenant_id');expect(raw).toContain('"amount_minor":9223372036854775807');expect(raw).toContain('"order_version":1');
  expect(f.calls.filter(r=>r.url.endsWith('/webhook'))[0]!.headers.has('X-Cart-Token')).toBe(false);
});
for(const fixture of [false,true])for(const pending of [false,true])test(`${fixture?'fixture':'backend'} ${pending?'pending':'processed'} receipt cannot mark paid; subsequent read owns result`,async()=>{
  const f=await setup(fixture);if(pending)f.pending();
  const response=await f.send();expect(response.status).toBe(200);
  expect(await response.json()).toMatchObject({state:'pending_payment',payment:{status:'authorized'}});
  expect(f.events).toHaveLength(1);expect(f.calls.at(-1)!.method).toBe('GET');
  expect(await (await f.completion()).json()).toMatchObject({state:'pending_payment'});
  f.apply();expect(await (await f.send()).json()).toMatchObject({state:'paid',payment:{status:'captured'}});
});
test('ambiguous fact retry keeps event identity and payload; terminal persisted read avoids another fact',async()=>{
  const f=await setup();await f.send();await f.send();
  expect(f.events).toHaveLength(2);expect(f.events[0]!.raw).toBe(f.events[1]!.raw);
  f.apply();await f.send();const count=f.events.length;await f.send();expect(f.events).toHaveLength(count);
});
test('browser cannot inject legacy tenant/account/time/signature or an authorized outcome',async()=>{
  const f=await setup();
  for(const extra of [{accepted:true},{accepted_total:{amount_minor:'9223372036854775807',currency:'USD',accepted:true}},{accepted_total:{amount_minor:9223372036854775807,currency:'USD'}},{tenant_id:tenant},{account_id:'foreign'},{timestamp:0},{signature:'forged'}])expect((await f.send('captured',extra)).status).toBe(400);
  expect((await f.send('authorized')).status).toBe(400);expect(f.events).toHaveLength(0);
});
test('rejected, malformed and mismatched receipts fail closed without promoting checkout',async()=>{
  const f=await setup();f.malformed();expect((await f.send()).status).toBe(502);
  expect(await (await f.completion()).json()).toMatchObject({state:'pending_payment'});
});
for(const [status,id,expected] of [['rejected',undefined,409],['paid',undefined,502],['processed','unrelated-event',502]] as const)test(`receipt ${status}/${id} cannot authorize completion`,async()=>{
  const f=await setup();f.receipt(status,id);expect((await f.send()).status).toBe(expected);
  expect(await (await f.completion()).json()).toMatchObject({state:'pending_payment'});
});
for(const status of [400,401,404,409])test(`backend protocol rejection ${status} is bounded and cannot become paid`,async()=>{
  const f=await setup();f.reject(status);const response=await f.send();
  expect(response.status).toBe(status);expect(await response.text()).not.toContain('private');
  expect(await (await f.completion()).json()).toMatchObject({state:'pending_payment'});
});
test('missing account and production Fake are closed before webhook IO',async()=>{
  const f=await setup();delete process.env.OPENSITE_FAKE_ACCOUNT_ID;
  expect((await f.send()).status).toBe(503);expect(f.events).toHaveLength(0);
  process.env.OPENSITE_FAKE_ACCOUNT_ID='acct_bff_test';process.env.OPENSITE_ENVIRONMENT='production';
  expect((await f.send()).status).toBe(404);expect(f.events).toHaveLength(0);
});

for (const accepted_total of [undefined,{amount_minor:'159',currency:'USD'},{amount_minor:'9223372036854775807',currency:'EUR'}]) test(`server rejects missing or stale capture consent ${JSON.stringify(accepted_total)}`,async()=>{
  const f=await setup();
  const response=await f.send('captured',{accepted_total});
  expect(response.status).toBe(409);expect(await response.json()).toMatchObject({code:'checkout_total_changed'});
  expect(f.events).toHaveLength(0);
});

test('matching accepted total replays exact capture bytes; changed acceptance never replays',async()=>{
  const f=await setup();await f.send();await f.send();
  expect(f.events[0]!.raw).toBe(f.events[1]!.raw);
  const rejected=await f.send('captured',{accepted_total:{amount_minor:'1',currency:'USD'}});
  expect(rejected.status).toBe(409);expect(f.events).toHaveLength(2);
  const start=f.calls.find(call=>call.url.endsWith('/checkout') && call.method==='POST')!;
  expect(await start.text()).toContain('"amount_minor":9223372036854775807');
});
