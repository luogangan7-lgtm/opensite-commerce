/** Purchasing BFF gates: production closed, capability custody, and same-origin session CSRF. */
import { afterEach, beforeEach, describe, expect, test, spyOn } from 'bun:test';
import type { AstroCookies } from 'astro';
import { fixtureConfiguration, handlePurchaseRequest, readPurchaseContext } from './commerce-server';
const site = 'http://127.0.0.1:4323', productID = 'c0900007-0000-4000-8000-000000000001', skuID = 'c09c0001-0000-4000-8000-000000000001';
const cartID = 'c09c0003-0000-4000-8000-000000000001', cap = 'Z'.repeat(43), etag = `"v1-${'a'.repeat(64)}"`;
function jar() {
  let value: string | undefined; let options: Record<string,unknown> = {};
  return { cookies: { get: () => value === undefined ? undefined : { value }, set: (_name: string, next: string, opts: Record<string,unknown>) => { value = next; options = opts; } } as unknown as AstroCookies,
    get value() { return value; }, get options() { return options; }, tamper() { value += 'x'; } };
}
const originalEnv = { mode: process.env.OSC_COMMERCE_FIXTURE_MODE, origin: process.env.OSC_COMMERCE_FIXTURE_ORIGIN, site: process.env.OSC_COMMERCE_FIXTURE_SITE };
let fetchMock: ReturnType<typeof spyOn> | undefined;
beforeEach(() => { process.env.OSC_COMMERCE_FIXTURE_MODE='contract-test'; process.env.OSC_COMMERCE_FIXTURE_ORIGIN='http://127.0.0.1:4176'; process.env.OSC_COMMERCE_FIXTURE_SITE=site; });
afterEach(() => { fetchMock?.mockRestore(); for (const [key,value] of Object.entries({OSC_COMMERCE_FIXTURE_MODE:originalEnv.mode,OSC_COMMERCE_FIXTURE_ORIGIN:originalEnv.origin,OSC_COMMERCE_FIXTURE_SITE:originalEnv.site})) { if(value===undefined) delete process.env[key]; else process.env[key]=value; } });
function upstream() {
  const calls: { url: string; init: RequestInit | undefined }[] = [];
  fetchMock = spyOn(globalThis,'fetch').mockImplementation(Object.assign(async (input: string | URL | Request, init?: RequestInit) => {
    const url=String(input); calls.push({url,init});
    if(url.endsWith('/__fixture/context')) return new Response(JSON.stringify({tenant_id:'c0900001-0000-4000-8000-000000000001',actor:'guest',products:[{product_id:productID,name:'Canvas tote',slug:'canvas-tote',skus:[{sku_id:skuID,sku_code:'TOTE',option_values:[],price_minor:'2400',currency:'USD',available:true}]}]}));
    const cart={id:cartID,currency:'USD',version:1,lines:[],subtotal:0};
    if(url.endsWith('/carts')) return new Response(JSON.stringify({cart,cart_token:cap}),{status:201,headers:{ETag:etag}});
    if(url.includes('/lines/')) return new Response(`{"id":"${cartID}","currency":"USD","version":2,"lines":[{"sku_id":"${skuID}","quantity":1,"sellable":true,"unit_amount":9223372036854775807,"line_amount":9223372036854775807}],"subtotal":9223372036854775807}`,{headers:{ETag:etag}});
    return new Response(JSON.stringify(cart),{headers:{ETag:etag}});
  }, { preconnect: globalThis.fetch.preconnect })); return calls;
}
describe('contract-test BFF',()=>{
  test('production and any non-exact loopback fixture configuration remain closed',async()=>{
    delete process.env.OSC_COMMERCE_FIXTURE_MODE;
    expect(fixtureConfiguration(new Request(site))).toBeNull();
    const response=await handlePurchaseRequest(new Request('https://shop.example/purchase/api/cart',{method:'POST'}),jar().cookies,'cart');
    expect(response.status).toBe(503); expect(await response.json()).toEqual({code:'purchase_unavailable'});
    process.env.OSC_COMMERCE_FIXTURE_MODE='contract-test';
    expect(fixtureConfiguration(new Request('https://shop.example/'))).toBeNull();
    process.env.OSC_COMMERCE_FIXTURE_ORIGIN='https://remote.example';
    expect(fixtureConfiguration(new Request(site))).toBeNull();
  });
  test('HttpOnly signed cookie custody strips the actual capability and keeps int64 exact',async()=>{
    const calls=upstream(), state=jar(); const context=await readPurchaseContext(new Request(site),state.cookies);
    expect(context.enabled).toBe(true); expect(state.options).toMatchObject({httpOnly:true,secure:true,sameSite:'lax',path:'/'});
    const response=await handlePurchaseRequest(new Request(`${site}/purchase/api/cart`,{method:'POST',headers:{Origin:site,'Content-Type':'application/json','X-CSRF-Token':context.csrf},body:JSON.stringify({sku_id:skuID,quantity:1})}),state.cookies,'cart');
    expect(response.status).toBe(200); const text=await response.text();
    expect(text).not.toContain(cap); expect(text).not.toContain('cart_token'); expect(JSON.parse(text).cart.subtotal).toBe('9223372036854775807');
    expect(calls.some(call=>new Headers(call.init?.headers).get('X-Cart-Token')===cap)).toBe(true);
    expect(text).not.toContain('available_quantity');
  });
  test('foreign Origin, absent/forged CSRF and tampered capability cookie cannot write upstream',async()=>{
    const calls=upstream(), state=jar(); const context=await readPurchaseContext(new Request(site),state.cookies); const before=calls.length;
    for(const [origin,csrf] of [['https://evil.example',context.csrf],[site,''],[site,'wrong']]) {
      const response=await handlePurchaseRequest(new Request(`${site}/purchase/api/cart`,{method:'POST',headers:{Origin:origin!,'Content-Type':'application/json','X-CSRF-Token':csrf!},body:JSON.stringify({sku_id:skuID,quantity:1})}),state.cookies,'cart'); expect(response.status).toBe(403);
    }
    state.tamper();
    const response=await handlePurchaseRequest(new Request(`${site}/purchase/api/cart`,{method:'POST',headers:{Origin:site,'Content-Type':'application/json','X-CSRF-Token':context.csrf},body:'{}'}),state.cookies,'cart');
    expect(response.status).toBe(403); expect(calls.length).toBe(before); expect(await response.text()).not.toContain('secret');
  });
});
