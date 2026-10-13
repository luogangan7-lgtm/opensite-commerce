/** Local browser transport fixtures for declared catalog/media, PR43 login and PR47 orders reads.
 * Playwright routes never reach external servers; these prove UI behavior, not backend auth integration. */
import {expect,type Page,type Route} from '@playwright/test';
export const tenant='11111111-1111-4111-8111-111111111111',productId='22222222-2222-4222-8222-222222222222',variantId='33333333-3333-4333-8333-333333333333',orderId='44444444-4444-4444-8444-444444444444',uploadId='55555555-5555-4555-8555-555555555555',mediaId='66666666-6666-4666-8666-666666666666';
const created='2026-10-12T12:00:00Z';
export const product={id:productId,product_code:'SAMPLE',lifecycle_status:'draft',version:1,created_at:created};
// PR47 snapshots have no product title, guest email or adjustment kind.
export const order={id:orderId,order_number:1001,state:'paid',version:1,currency:'USD',lines:[{line_id:variantId,sku_code:'SAMPLE-1',title:null,quantity:2,unit_price_minor:'1000',tax_minor:'200',line_total_minor:'2200'}],adjustments:[{id:mediaId,reason:'Manual adjustment',amount_minor:'-100'}],shipping:{amount_minor:'500',method:'fixed'},tax_total_minor:'200',total_minor:'2600',created_at:created,updated_at:created};
export async function fixture(page:Page,options:{authenticated?:boolean;loginStatus?:number;productConflict?:boolean;uploadFailure?:boolean;uploadStatus?:'ready'|'processing'|'failed';ordersFailure?:number;orderMissing?:boolean;orderBadETag?:boolean;shippingMethod?:string|null;empty?:boolean}={}){
 let authenticated=options.authenticated??true,current={...product},variants:{id:string;product_id:string;sku:string;version:number;lifecycle_status:string;option_value_ids:string[];created_at:string}[]=[];
 let ordersFailure=options.ordersFailure;
 const writes:string[]=[];const csrf=crypto.randomUUID(),challenge=crypto.randomUUID();let expires=new Date(Date.now()+3600000).toISOString();
 async function json(route:Route,data:unknown,status=200,headers:Record<string,string>={}){await route.fulfill({status,contentType:'application/json',body:JSON.stringify(data),headers});}
 async function problem(route:Route,status:number){await route.fulfill({status,contentType:'application/problem+json',body:JSON.stringify({type:'about:blank',title:'Rejected',status,code:status===412?'version_conflict':'unauthenticated',detail:'PRIVATE SERVER DETAIL NEVER DISPLAYED'})});}
 await page.route('**/api/v1/**',async route=>{const r=route.request(),url=new URL(r.url()),path=url.pathname,method=r.method();
  if(path==='/api/v1/auth/login'&&method==='GET')return json(route,{csrf_token:challenge},200,{'Set-Cookie':`__Host-osc_login_csrf=${crypto.randomUUID()}; Secure; HttpOnly; SameSite=Lax; Path=/`});
  if(path==='/api/v1/auth/login'&&method==='POST'){expect(r.headers().origin).toBe(url.origin);expect(r.headers().cookie).toContain('__Host-osc_login_csrf=');expect(r.headers()['x-csrf-token']).toBe(challenge);if(options.loginStatus)return problem(route,options.loginStatus);authenticated=true;return json(route,{user_id:tenant,session_id:variantId,expires_at:expires,csrf_token:csrf},201,{'Set-Cookie':`__Host-osc_admin_session=${crypto.randomUUID()}; Secure; HttpOnly; SameSite=Lax; Path=/`});}
  if(path==='/api/v1/auth/session'){if(!authenticated)return problem(route,401);return json(route,{user_id:tenant,session_id:variantId,expires_at:expires,csrf_token:csrf});}
  if(!authenticated)return problem(route,401);
  if(method!=='GET'){expect(r.headers().origin).toBe(url.origin);expect(r.headers()['x-csrf-token']).toBe(csrf);if(path!=='/api/v1/auth/logout')expect(r.headers()['idempotency-key']).toBeTruthy();writes.push(method+' '+path);}
  if(path==='/api/v1/auth/logout'){authenticated=false;return route.fulfill({status:204,headers:{'Set-Cookie':'__Host-osc_admin_session=; Secure; HttpOnly; Path=/; Max-Age=0'}});}
  if(!path.startsWith(`/api/v1/t/${tenant}/`))return problem(route,404);
  const base=`/api/v1/t/${tenant}`;
  if(path===`${base}/products`){if(method==='POST'){current={...current,product_code:r.postDataJSON().product_code.toUpperCase()};return json(route,current,201);}return json(route,{items:options.empty||url.searchParams.has('cursor')?[]:[current],...(!options.empty&&!url.searchParams.has('cursor')?{next_cursor:'fixture cursor/+?'}:{})});}
  if(path===`${base}/products/${productId}`){if(method==='PATCH'){expect(r.headers()['if-match']).toBe(`"${current.version}"`);if(options.productConflict)return problem(route,412);current={...current,...r.postDataJSON(),version:current.version+1};}return json(route,current);}
  if(path===`${base}/products/${productId}/variants`){if(method==='POST'){variants.push({id:variantId,product_id:productId,...r.postDataJSON(),lifecycle_status:'draft',version:1,created_at:created});return json(route,variants.at(-1),201);}return json(route,{items:variants});}
  if(path===`${base}/products/${productId}/variants/${variantId}`){if(method==='PATCH'){expect(r.headers()['if-match']).toBe(`"${variants[0].version}"`);variants[0]={...variants[0],...r.postDataJSON(),version:variants[0].version+1};}if(method==='DELETE'){expect(r.headers()['if-match']).toBe(`"${variants[0].version}"`);variants[0].lifecycle_status='archived';return route.fulfill({status:204});}return json(route,variants[0]);}
  if(path===`${base}/media/uploads`){const body=r.postDataJSON();expect(body.sha256).toMatch(/^[a-f0-9]{64}$/);expect(body.size_bytes).toBeGreaterThan(0);return json(route,{id:uploadId,status:'reserved',expires_at:expires},201);}
  if(path===`${base}/media/uploads/${uploadId}/content`){expect(r.headers()['content-type']).toBe('application/octet-stream');expect(r.postDataBuffer()?.length).toBeGreaterThan(0);return route.fulfill({status:204});}
  if(path===`${base}/media/uploads/${uploadId}/complete`){if(options.uploadFailure)return problem(route,503);return json(route,{id:uploadId,status:options.uploadStatus??'ready',media_id:mediaId,expires_at:expires},202);}
  if(path===`${base}/products/${productId}/media`)return json(route,{product_id:productId,...r.postDataJSON()},201);
  if(path===`${base}/orders`){expect(method).toBe('GET');if(ordersFailure)return problem(route,ordersFailure);const limit=Number(url.searchParams.get('limit')??25);expect(limit).toBeGreaterThanOrEqual(1);expect(limit).toBeLessThanOrEqual(100);const empty=options.empty||url.searchParams.has('cursor')||(url.searchParams.has('state')&&url.searchParams.get('state')!==order.state);return json(route,{items:empty?[]:[{id:orderId,order_number:order.order_number,state:order.state,currency:order.currency,total_minor:order.total_minor,customer_kind:'guest',created_at:created}],...(!empty?{next_cursor:'orders-page-2'}:{})});}
  if(path===`${base}/orders/${orderId}`){expect(method).toBe('GET');if(options.orderMissing)return problem(route,404);return json(route,{...order,shipping:{...order.shipping,method:options.shippingMethod===undefined?order.shipping.method:options.shippingMethod}},200,{ETag:options.orderBadETag?'"2"':'"1"'});}
  return problem(route,404);
 });
 return {writes,recoverOrders:()=>{ordersFailure=undefined;},expire:()=>{authenticated=false;expires=created;}};
}
