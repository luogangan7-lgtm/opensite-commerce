/** Browser transport receipt: same-origin, explicit preconditions, generic errors and no write retry. */
import { afterEach, expect, test, spyOn } from 'bun:test';
import { CommerceError, getCart, setCartLine, startCheckout } from './commerce-client';
let mock: ReturnType<typeof spyOn> | undefined;
afterEach(()=>mock?.mockRestore());
function transport(response: Response) {
 const calls: {url: string; init?: RequestInit}[]=[];
 mock=spyOn(globalThis,'fetch').mockImplementation(Object.assign(async(input: RequestInfo | URL,init?: RequestInit)=>{calls.push({url:String(input),...(init?{init}:{})});return response.clone();},{preconnect:globalThis.fetch.preconnect}));return calls;
}
test('cart absolute quantities carry exact ETag and session CSRF without browser capabilities',async()=>{
 const calls=transport(new Response(JSON.stringify({cart:{id:'id',currency:'USD',version:'2',lines:[],subtotal:'9223372036854775807'},etag:'"etag"'})));
 const result=await setCartLine('sku',3,'"strong-v1"','csrf');
 expect(result.cart.subtotal).toBe('9223372036854775807');expect(calls).toHaveLength(1);
 expect(calls[0]!.url).toBe('/purchase/api/lines/sku');expect(calls[0]!.init).toMatchObject({method:'PUT',credentials:'same-origin',cache:'no-store',redirect:'error'});
 const headers=new Headers(calls[0]!.init?.headers); expect(headers.get('If-Match')).toBe('"strong-v1"');expect(headers.get('X-CSRF-Token')).toBe('csrf');expect(headers.has('X-Cart-Token')).toBe(false);
 expect(calls[0]!.init?.body).toBe('{"quantity":3}');
});
test('writes do not auto-retry or surface internal problem details',async()=>{
 const calls=transport(new Response('{"detail":"postgres://secret/internal-error"}',{status:412}));
 try {await setCartLine('sku',3,'"old"','csrf');throw new Error('expected failure');} catch(error){expect(error).toBeInstanceOf(CommerceError);expect((error as CommerceError).status).toBe(412);expect((error as Error).message).not.toContain('secret');}
 expect(calls).toHaveLength(1);
});
test('checkout stable idempotency header and guest body forwarded, reads sameorigin',async()=>{
 const calls=transport(new Response('{}'));
 await startCheckout({shipping_address:{country:'US',address1:'1 Test Way',city:'City',postal_code:'10001'},buyer_contact:{email:'buyer@example.test'}},'stable-key','csrf');
 expect(new Headers(calls[0]!.init?.headers).get('Idempotency-Key')).toBe('stable-key');
 expect(calls[0]!.url).toBe('/purchase/api/checkout');
 await getCart(); expect(calls[1]!.url).toBe('/purchase/api/cart'); expect(calls[1]!.init?.credentials).toBe('same-origin');
});
