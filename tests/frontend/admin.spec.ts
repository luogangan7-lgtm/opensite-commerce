/** Exercises production UI and pending-contract fixtures; Playwright/axe prove UI behavior only. CI alone writes snapshots. */
import {test,expect,type Page} from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import {fixture,tenant,productId,orderId,order} from './admin-fixture';
const origin=process.env.OPENSITE_ADMIN_E2E_ORIGIN??'http://127.0.0.1:4174';
const fixtureOrigin=process.env.OPENSITE_ADMIN_FIXTURE_ORIGIN??'http://127.0.0.1:4173';
const snapshots=process.env.OPENSITE_ADMIN_NO_SNAPSHOTS!=='1';
async function audit(page:Page,name?:string){await page.evaluate(async()=>{await document.fonts.ready;await Promise.all(document.getAnimations().filter(a=>a.effect?.getComputedTiming().iterations!==Infinity).map(a=>a.finished.catch(()=>{})));});expect((await new AxeBuilder({page}).analyze()).violations).toEqual([]);expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);if(name&&snapshots)await expect(page).toHaveScreenshot(name,{fullPage:true});}
// Hold one routed API response until its loading UI has been captured; no backend/time changes.
async function holdResponse(page:Page,url:string){
 let release!:()=>void,started!:()=>void;
 const ready=new Promise<void>(resolve=>{started=resolve;}),pending=new Promise<void>(resolve=>{release=resolve;});
 await page.route(url,async route=>{started();await pending;await route.fallback();},{times:1});
 return {ready,release};
}
async function signIn(page:Page){await page.getByLabel('Email').fill('staff@example.invalid');await page.getByLabel('Password',{exact:true}).fill(crypto.randomUUID());await page.getByRole('button',{name:'Sign in',exact:true}).click();}
test('merchant data privacy explains support deletion without offering a write action',async({page})=>{
 await fixture(page);await page.goto(`${origin}/t/${tenant}/products`);
 await page.getByRole('navigation',{name:'Store navigation'}).getByRole('link',{name:'Data privacy',exact:true}).click();
 await expect(page).toHaveURL(new RegExp(`/t/${tenant}/data-privacy$`));
 await expect(page.getByRole('heading',{name:'Data privacy',exact:true})).toBeFocused();
 const main=page.getByRole('main');
 await expect(main.getByText('To delete shop or customer data, contact support. We will process it within 30 days and provide an export copy.',{exact:true})).toBeVisible();
 const chinese=main.getByText('如需删除店铺或客户数据，请联系支持，我们会在 30 天内处理并提供导出副本。',{exact:true});
 await expect(chinese).toBeVisible();await expect(chinese).toHaveAttribute('lang','zh-CN');
 await expect(main.getByRole('button')).toHaveCount(0);await expect(main.getByRole('form')).toHaveCount(0);
 const writes:string[]=[];page.on('request',r=>{if(!['GET','HEAD','OPTIONS'].includes(r.method()))writes.push(r.method()+' '+r.url());});
 await page.reload();await expect(main.getByRole('heading',{name:'Data privacy',exact:true})).toBeVisible();
 expect(writes).toEqual([]);await audit(page,'admin-data-privacy.png');
});
test('merchant data privacy remains behind the session guard',async({page})=>{
 await fixture(page,{authenticated:false});await page.goto(`${origin}/t/${tenant}/data-privacy`);
 await expect(page).toHaveURL(/\/login/);await expect(page.getByRole('heading',{name:'Sign in',exact:true})).toBeVisible();
 await expect(page.getByRole('heading',{name:'Data privacy',exact:true})).toHaveCount(0);
});
test('order table keeps identifiers and money readable inside a keyboard scroll region',async({page})=>{
  await fixture(page);await page.goto(`${fixtureOrigin}/t/${tenant}/orders/44444444-4444-4444-8444-444444444444`);
  const table=page.getByRole('table',{name:'Order lines'});await expect(table.getByRole('cell',{name:'USD 22.00',exact:true})).toBeVisible();
  for(const cell of [table.getByRole('cell',{name:'SAMPLE-1',exact:true}),table.getByRole('cell',{name:'USD 22.00',exact:true}),table.getByRole('columnheader',{name:'Quantity',exact:true})]){
    expect(await cell.evaluate(el=>{const range=document.createRange();range.selectNodeContents(el);return range.getClientRects().length;})).toBe(1);
  }
  const region=page.getByRole('region',{name:'Order lines'});await region.focus();await expect(region).toBeFocused();
  if(await region.evaluate(el=>el.scrollWidth>el.clientWidth)){
    await page.keyboard.press('ArrowRight');await expect.poll(()=>region.evaluate(el=>el.scrollLeft)).toBeGreaterThan(0);
  }
  await audit(page);
});
test('login page labels and route guard',async({page})=>{await fixture(page,{authenticated:false});await page.goto(`${origin}/t/${tenant}/products`);await expect(page.getByRole('heading',{name:'Sign in'})).toBeVisible();await expect(page.getByLabel('Email')).toBeVisible();await expect(page.getByLabel('Password',{exact:true})).toBeVisible();await expect(page).toHaveURL(/\/login/);await audit(page,'admin-login.png');});
test('generic login failure and throttling never expose account details',async({page})=>{await fixture(page,{authenticated:false,loginStatus:401});await page.goto(`${origin}/login`);await signIn(page);await expect(page.getByRole('alert')).toHaveText('Unable to sign in. Check your credentials or try again later.');await expect(page.getByLabel('Password',{exact:true})).toHaveValue('');await expect(page.getByText('PRIVATE SERVER DETAIL')).toHaveCount(0);await audit(page);await page.unrouteAll({behavior:'wait'});await fixture(page,{authenticated:false,loginStatus:429});await page.reload();await signIn(page);await expect(page.getByRole('alert')).toContainText('temporarily limited');await audit(page);});
test('product cursor paging and empty state',async({page})=>{await fixture(page);await page.goto(`${origin}/t/${tenant}/products`);await expect(page.getByRole('link',{name:'SAMPLE',exact:true})).toBeVisible();await audit(page,'admin-products.png');await page.getByRole('button',{name:'Next page',exact:true}).click();await expect(page.getByText('No products on this page')).toBeVisible();await page.getByRole('button',{name:'Previous page',exact:true}).click();await expect(page.getByRole('link',{name:'SAMPLE',exact:true})).toBeVisible();});
test('new product validation and creation',async({page})=>{await fixture(page);await page.goto(`${origin}/t/${tenant}/products/new`);await expect(page.getByRole('heading',{name:'Create product',exact:true})).toBeVisible();await audit(page,'admin-create.png');await page.getByLabel('Product code',{exact:true}).fill('BAD SPACE');await page.getByRole('button',{name:'Create product',exact:true}).click();expect(await page.getByLabel('Product code',{exact:true}).evaluate(e=>(e as HTMLInputElement).validity.valid)).toBe(false);await page.getByLabel('Product code',{exact:true}).fill('NEW-ITEM');await page.getByRole('button',{name:'Create product',exact:true}).click();await expect(page.getByRole('heading',{name:'Edit product'})).toBeVisible();});
test('edit conflict retains draft and SKU/upload errors stay actionable',async({page})=>{const state=await fixture(page,{productConflict:true,uploadFailure:true});await page.goto(`${origin}/t/${tenant}/products/${productId}`);await expect(page.getByRole('heading',{name:'Edit product'})).toBeVisible();await audit(page,'admin-edit.png');await page.getByLabel('Product code',{exact:true}).fill('UNSAVED');await page.getByRole('button',{name:'Save product',exact:true}).click();await expect(page.getByText('This resource changed. Reload it before saving.')).toBeVisible();await expect(page.getByLabel('Product code',{exact:true})).toHaveValue('UNSAVED');await page.getByRole('button',{name:'Reload product from server',exact:true}).click();await expect(page.getByLabel('Product code',{exact:true})).toHaveValue('SAMPLE');await page.getByLabel('SKU code',{exact:true}).fill('SAMPLE-1');await page.getByRole('button',{name:'Create SKU',exact:true}).click();await expect(page.getByRole('button',{name:'Edit SAMPLE-1'})).toBeVisible();await page.getByLabel('Media file').setInputFiles({name:'sample.png',mimeType:'image/png',buffer:Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg==','base64')});await page.getByRole('button',{name:'Upload media',exact:true}).click();await expect(page.getByText('The service is temporarily unavailable. Try again later.')).toBeVisible();expect(state.writes.some(x=>x.endsWith('/complete'))).toBe(true);await audit(page);});
test('production orders require authentication and never fall back to fixtures',async({page})=>{
 await fixture(page,{authenticated:false});let reads=0;page.on('request',r=>{if(/\/api\/v1\/.*\/orders/.test(r.url()))reads++;});
 await page.goto(`${origin}/t/${tenant}/orders`);await expect(page).toHaveURL(/\/login/);expect(reads).toBe(0);
 await page.unrouteAll({behavior:'wait'});await fixture(page,{ordersFailure:503});await page.goto(`${origin}/t/${tenant}/orders`);
 await expect(page.getByRole('alert')).toContainText('temporarily unavailable');expect(reads).toBeGreaterThan(0);
 await expect(page.getByText('CONTRACT FIXTURE')).toHaveCount(0);await expect(page.getByRole('link',{name:'1001'})).toHaveCount(0);await audit(page);
});
test('orders contract fixture list and detail snapshots',async({page})=>{await fixture(page);await page.goto(`${fixtureOrigin}/t/${tenant}/orders`);await expect(page.getByText('CONTRACT FIXTURE (orders read API pending PR)')).toBeVisible();await expect(page.getByRole('link',{name:'1001'})).toBeVisible();await audit(page,'admin-orders.png');await page.getByRole('link',{name:'1001'}).click();await expect(page.getByRole('heading',{name:'Order 1001'})).toBeVisible();await expect(page.getByText('USD 26.00')).toBeVisible();await expect(page.getByText('Shipping rule (fixed)')).toBeVisible();await expect(page.getByRole('cell',{name:'SAMPLE-1',exact:true})).toBeVisible();await expect(page.getByRole('columnheader',{name:'Item',exact:true})).toHaveCount(0);await expect(page.getByText('USD -1.00',{exact:true})).toBeVisible();await expect(page.getByRole('button',{name:/refund|ship/i})).toHaveCount(0);await audit(page,'admin-order.png');});
test('login create upload order flow and expiry cleanup',async({page})=>{const state=await fixture(page,{authenticated:false});await page.goto(`${fixtureOrigin}/t/${tenant}/products`);await signIn(page);await expect(page.getByRole('heading',{name:'Products',exact:true})).toBeVisible();const cookies=await page.context().cookies();expect(cookies.some(c=>c.name==='__Host-osc_admin_session'&&c.httpOnly&&c.secure&&c.path==='/')).toBe(true);await page.getByRole('link',{name:'Create product',exact:true}).click();await page.getByLabel('Product code',{exact:true}).fill('FLOW-ITEM');await page.getByRole('button',{name:'Create product',exact:true}).click();await expect(page.getByRole('heading',{name:'Edit product'})).toBeVisible();await page.getByLabel('Media file').setInputFiles({name:'sample.png',mimeType:'image/png',buffer:Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg==','base64')});await page.getByRole('button',{name:'Upload media',exact:true}).click();await expect(page.getByText('ready',{exact:true})).toBeVisible();await page.getByRole('button',{name:'Attach media',exact:true}).click();await expect(page.getByText('Media attached to this product.')).toBeVisible();await page.getByRole('link',{name:'Orders',exact:true}).click();await page.getByRole('link',{name:'1001'}).click();await expect(page.getByRole('heading',{name:'Order 1001'})).toBeVisible();expect(state.writes.length).toBeGreaterThan(3);state.expire();await page.goto(`${fixtureOrigin}/t/${tenant}/products`);await expect(page.getByRole('heading',{name:'Sign in'})).toBeVisible();await expect(page.getByText('FLOW-ITEM')).toHaveCount(0);expect(await page.evaluate(()=>Object.keys(localStorage))).toEqual([]);});
test('logout revokes session and no private content remains',async({page})=>{await fixture(page);await page.goto(`${origin}/t/${tenant}/products`);await expect(page.getByRole('heading',{name:'Products',exact:true})).toBeVisible();await page.getByRole('button',{name:'Sign out',exact:true}).click();await expect(page.getByRole('heading',{name:'Sign in'})).toBeVisible();await expect(page.getByText('SAMPLE',{exact:true})).toHaveCount(0);});

test('SKU edit and archive preserve version preconditions',async({page})=>{await fixture(page);await page.goto(`${origin}/t/${tenant}/products/${productId}`);await page.getByLabel('SKU code',{exact:true}).fill('FIRST-SKU');await page.getByRole('button',{name:'Create SKU',exact:true}).click();await page.getByRole('button',{name:'Edit FIRST-SKU'}).click();await page.getByLabel('SKU code',{exact:true}).fill('UPDATED-SKU');await page.getByRole('button',{name:'Save SKU',exact:true}).click();await expect(page.getByRole('button',{name:'Edit UPDATED-SKU'})).toBeVisible();await page.getByRole('button',{name:'Archive UPDATED-SKU'}).click();await expect(page.getByRole('button',{name:'Archive UPDATED-SKU'})).toBeDisabled();await audit(page);});

test('orders paging, state filter and retry use the read contract',async({page})=>{const state=await fixture(page,{ordersFailure:503});await page.goto(`${fixtureOrigin}/t/${tenant}/orders`);await expect(page.getByRole('alert')).toContainText('temporarily unavailable');state.recoverOrders();await page.getByRole('button',{name:'Retry',exact:true}).click();await page.getByRole('link',{name:'1001'}).waitFor();await page.getByRole('button',{name:'Next page',exact:true}).click();await expect(page.getByText('No orders match this filter.')).toBeVisible();await page.getByRole('button',{name:'Previous page',exact:true}).click();await page.getByRole('link',{name:'1001'}).waitFor();await page.getByLabel('Order state').selectOption('cancelled');await expect(page.getByText('No orders match this filter.')).toBeVisible();await audit(page);});
test('orders unavailable in another tenant and version errors are safe',async({page})=>{await fixture(page,{orderMissing:true});await page.goto(`${fixtureOrigin}/t/${tenant}/orders/99999999-9999-4999-8999-999999999999`);await expect(page.getByRole('alert')).toContainText('not found or unavailable');await expect(page.getByText('PRIVATE SERVER DETAIL')).toHaveCount(0);await audit(page);await page.unrouteAll({behavior:'wait'});await fixture(page,{orderBadETag:true});await page.goto(`${fixtureOrigin}/t/${tenant}/orders/44444444-4444-4444-8444-444444444444`);await expect(page.getByRole('alert')).toContainText('Unable to load orders');await audit(page);await page.goto(`${fixtureOrigin}/t/99999999-9999-4999-8999-999999999999/orders`);await expect(page.getByRole('alert')).toBeVisible();await expect(page.getByRole('link',{name:'1001'})).toHaveCount(0);});
test('upload processing and failed states are explicit',async({page})=>{await fixture(page,{uploadStatus:'processing'});await page.goto(`${origin}/t/${tenant}/products/${productId}`);await page.getByLabel('Media file').setInputFiles({name:'sample.png',mimeType:'image/png',buffer:Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg==','base64')});await page.getByRole('button',{name:'Upload media',exact:true}).click();await expect(page.getByText('processing',{exact:true})).toBeVisible();await audit(page);await page.unrouteAll({behavior:'wait'});await fixture(page,{uploadStatus:'failed'});await page.reload();await page.getByLabel('Media file').setInputFiles({name:'sample.png',mimeType:'image/png',buffer:Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg==','base64')});await page.getByRole('button',{name:'Upload media',exact:true}).click();await expect(page.getByRole('alert')).toContainText('Media verification or processing failed');await expect(page.getByRole('button',{name:'Attach media',exact:true})).toBeDisabled();await audit(page);});

test('PR47 null shipping method remains readable with SKU-only snapshot lines',async({page})=>{await fixture(page,{shippingMethod:null});await page.goto(`${fixtureOrigin}/t/${tenant}/orders/44444444-4444-4444-8444-444444444444`);await expect(page.getByRole('heading',{name:'Order 1001'})).toBeVisible();await expect(page.getByRole('cell',{name:'SAMPLE-1',exact:true})).toBeVisible();await expect(page.getByText('Shipping rule (Not recorded)')).toBeVisible();await expect(page.getByText('Sample product',{exact:true})).toHaveCount(0);await expect(page.getByRole('alert')).toHaveCount(0);await audit(page);});

test('price keyboard submission associates and announces invalid amounts', async ({ page }) => {
 await fixture(page);
 const list = { id: tenant, version: 1, currency: 'USD', status: 'active', audience: 'all', priority: 0,
   starts_at: '2020-01-01T00:00:00Z', ends_at: null };
 let writes = 0, priceStatus = 200;
 await page.route('**/api/v1/t/*/price-lists**', async route => {
   if (route.request().method() === 'PUT') {
     writes++;
     expect(route.request().headers()['if-match']).toBe('"1"');
     if(priceStatus!==200)return route.fulfill({status:priceStatus,json:{status:priceStatus,code:'version_conflict'}});
   }
   await route.fulfill({ json: new URL(route.request().url()).pathname.endsWith('/price-lists') ? { items: [list] } : list });
 });
 await page.goto(`${origin}/t/${tenant}/products/${productId}`);
 await page.getByLabel('SKU code', { exact: true }).fill('PRICE-SKU');
 await page.getByRole('button', { name: 'Create SKU', exact: true }).click();
 await page.getByRole('button', { name: 'Edit PRICE-SKU', exact: true }).click();
 await page.getByLabel('Price list', { exact: true }).selectOption(tenant);
 const amount = page.getByLabel('Price in minor units', { exact: true });
 await expect(amount).toBeEnabled();
 for (const invalid of ['', '-1', '9223372036854775808']) {
   await amount.fill(invalid);
   await amount.press('Enter');
   await expect(amount).toBeFocused();
   await expect(amount).toHaveAttribute('aria-invalid', 'true');
   await expect(amount).toHaveAttribute('aria-describedby', 'price-minor-help price-minor-error');
   await expect(page.locator('#price-minor-error')).toHaveAttribute('role', 'alert');
   await expect(amount).toHaveAccessibleDescription(/Enter whole minor units from 0 to 9223372036854775807\./);
   expect(writes).toBe(0);
 }
 await audit(page,'admin-price-invalid.png');
 await page.getByRole('button', { name: 'Reload price from server', exact: true }).click();
 await expect(amount).toBeEnabled();
 await expect(amount).toHaveValue('');
 await expect(page.locator('#price-minor-error')).toHaveCount(0);
 await amount.fill('3456');
 await expect(amount).not.toHaveAttribute('aria-invalid', 'true');
 await expect(amount).toHaveAttribute('aria-describedby', 'price-minor-help');
 await expect(page.locator('#price-minor-error')).toHaveCount(0);
 await amount.press('Enter');
 await expect(page.getByRole('status').filter({ hasText: 'Price saved' })).toBeVisible();
 expect(writes).toBe(1);
 priceStatus=412;await amount.fill('9223372036854775807');await amount.press('Enter');
 await expect(page.getByRole('button',{name:'Save price',exact:true})).toBeDisabled();
 await expect(amount).toHaveValue('9223372036854775807');
 await expect(page.getByRole('alert')).toContainText('This resource changed');
 await audit(page,'admin-price-conflict.png');
});

test('content and listing preserve opaque ETags and retain conflicting drafts',async({page})=>{
 await fixture(page);await page.goto(`${origin}/t/${tenant}/products/${productId}`);
 await page.getByRole('button',{name:'Load content',exact:true}).click();
 await expect(page.getByLabel('Title',{exact:true})).toHaveValue('Sample title');
 await page.getByLabel('Title',{exact:true}).fill('Unsaved title');
 const saving=await holdResponse(page,'**/api/v1/t/*/products/*/content/en');
 await page.getByRole('button',{name:'Save content',exact:true}).click();
 await saving.ready;
 try{await expect(page.getByLabel('Title',{exact:true})).toBeDisabled();await audit(page,'admin-publication-saving.png');}finally{saving.release();}
 await expect(page.getByRole('button',{name:'Save content',exact:true})).toBeDisabled();
 await expect(page.getByRole('alert')).toContainText('This resource changed');
 await expect(page.getByLabel('Title',{exact:true})).toHaveValue('Unsaved title');
 await audit(page,'admin-content-conflict.png');
 await page.getByRole('button',{name:'Load content',exact:true}).click();
 await expect(page.getByLabel('Title',{exact:true})).toHaveValue('Sample title');
 await page.getByLabel('Site ID',{exact:true}).fill(tenant);
 await page.getByRole('button',{name:'Load listing',exact:true}).click();
 await page.getByLabel('Slug',{exact:true}).fill('unsaved-slug');
 await page.getByRole('button',{name:'Save listing',exact:true}).click();
 await expect(page.getByRole('button',{name:'Save listing',exact:true})).toBeDisabled();
 await expect(page.getByLabel('Slug',{exact:true})).toHaveValue('unsaved-slug');
 await audit(page,'admin-publication-conflict.png');
 await page.getByRole('button',{name:'Load listing',exact:true}).click();
 await expect(page.getByLabel('Slug',{exact:true})).toHaveValue('sample');
 await expect(page.getByRole('button',{name:'Save listing',exact:true})).toBeEnabled();
 await page.getByLabel('Title',{exact:true}).fill('A detailed multilingual product title 商品说明 '.repeat(8));
 await page.getByLabel('Body blocks JSON',{exact:true}).fill(JSON.stringify([{type:'paragraph',text:'Product content details 商品内容 '.repeat(20)}]));
 await page.getByLabel('Slug',{exact:true}).fill('long-product-publication-slug-'.repeat(6).slice(0,-1));
 await audit(page,'admin-publication-long.png');
 await page.route('**/api/v1/t/*/products/*/content/en',route=>route.fulfill({status:503,json:{detail:'PRIVATE SERVER DETAIL'}}));
 await page.getByRole('button',{name:'Load content',exact:true}).click();
 await expect(page.getByRole('alert')).toContainText('temporarily unavailable');
 await expect(page.getByText('PRIVATE SERVER DETAIL')).toHaveCount(0);
 await audit(page,'admin-publication-error.png');
});

test('notification snapshot pages and confirmation retain safe recovery states',async({page})=>{
 const id='c3400001-0000-4000-8000-000000000001';
 const failure={id,event_id:id,template_key:'order.shipped',status:'dead_letter',attempts:10,generation:1,created_at:'2026-01-01T00:00:00Z',updated_at:'2026-01-01T00:00:00Z',safe_error:'NOTIFICATION_SEND_FAILED'};
 const path=`${origin}/t/${tenant}/notifications`;
 await fixture(page);let status=503;const keys:string[]=[];
 const rows=Array.from({length:12},(_,i)=>({...failure,id:`c3400001-0000-4000-8000-${String(i+1).padStart(12,'0')}`,status:i===1?'uncertain':'dead_letter'}));
 await page.route('**/notifications/**',async route=>{
  if(route.request().method()==='GET')return route.fulfill({json:{items:rows}});
  keys.push(route.request().headers()['idempotency-key']);
  return route.fulfill({status,contentType:'application/problem+json',json:{code:'PRIVATE_CONTACT_DETAIL',detail:'PRIVATE_CONTACT_DETAIL'}});
 });
 const loading=await holdResponse(page,'**/api/v1/t/*/notifications/failures?*');
 await page.goto(path);await loading.ready;
 try{await expect(page.getByRole('button',{name:'Reload failures',exact:true})).toBeDisabled();await audit(page,'admin-notifications-loading.png');}finally{loading.release();}
 await expect(page.getByRole('table',{name:'Notification failures'}).getByRole('row')).toHaveCount(11);
 await expect(page.getByText('Investigate manually',{exact:true})).toBeVisible();
 await page.getByRole('button',{name:'Next page',exact:true}).click();
 await expect(page.getByRole('table',{name:'Notification failures'}).getByRole('row')).toHaveCount(3);
 await page.getByRole('button',{name:'Previous page',exact:true}).click();
 await audit(page,'admin-notifications.png');
 const region=page.getByRole('region',{name:'Notification failures'});
 await region.focus();await expect(region).toBeFocused();
 const overflow=await region.evaluate(el=>el.scrollWidth>el.clientWidth);
 if(page.viewportSize()!.width===390)expect(overflow).toBe(true);
 if(overflow){await page.keyboard.press('ArrowRight');await expect.poll(()=>region.evaluate(el=>el.scrollLeft)).toBeGreaterThan(0);await region.evaluate(el=>{el.scrollLeft=el.scrollWidth;});}
 await expect(region.getByRole('button',{name:'Redrive',exact:true}).first()).toBeInViewport();
 await audit(page,'admin-notifications-recovery.png');
 const trigger=page.getByRole('row').filter({hasText:id}).getByRole('button',{name:'Redrive',exact:true});
 await trigger.focus();await page.keyboard.press('Enter');
 const dialog=page.getByRole('dialog',{name:'Redrive notification?'});
 await expect(dialog.getByRole('button',{name:'Cancel redrive',exact:true})).toBeFocused();
 await audit(page,'admin-notifications-confirmation.png');
 await page.keyboard.press('Escape');await expect(trigger).toBeFocused();expect(keys).toEqual([]);
 await trigger.click();const confirming=await holdResponse(page,'**/api/v1/t/*/notifications/*/redrive');
 await dialog.getByRole('button',{name:'Confirm redrive',exact:true}).click();await confirming.ready;
 try{await expect(dialog.getByRole('button',{name:'Cancel redrive',exact:true})).toBeDisabled();await audit(page,'admin-notifications-confirming.png');}finally{confirming.release();}
 await expect(dialog.getByRole('alert')).toContainText('Retry uses the same request key');
 await audit(page,'admin-notifications-error.png');
 status=409;await dialog.getByRole('button',{name:'Confirm redrive',exact:true}).click();
 await expect(page.getByRole('status').filter({hasText:'state changed'})).toBeVisible();
 expect(keys).toHaveLength(2);expect(keys[0]).toBe(keys[1]);
 await expect(trigger).toBeDisabled();await expect(page.getByText('PRIVATE_CONTACT_DETAIL')).toHaveCount(0);
 await audit(page,'admin-notifications-stale.png');
 await page.getByRole('button',{name:'Reload failures',exact:true}).click();await expect(trigger).toBeEnabled();
});
test('notification 403 hides all private metadata and mutation controls',async({page})=>{
 const path=`${origin}/t/${tenant}/notifications`;
 await fixture(page);
 await page.route('**/notifications/**',route=>route.fulfill({status:403,json:{detail:'PRIVATE_CONTACT_DETAIL'}}));
 await page.goto(path);
 await expect(page.getByText('Notification recovery is unavailable for this session.',{exact:true})).toBeVisible();
 await expect(page.getByRole('button',{name:'Redrive',exact:true})).toHaveCount(0);
 await expect(page.getByRole('table')).toHaveCount(0);
 await expect(page.getByText('PRIVATE_CONTACT_DETAIL')).toHaveCount(0);
 await audit(page,'admin-notifications-forbidden.png');
});

// Production build + routed contract responses: visual UI evidence only, not backend refund/auth E2E.
test('production orders list and detail expose loading, empty and safe retry states',async({page})=>{
 await fixture(page);let status=200,empty=false;
 await page.route('**/api/v1/t/*/orders**',async route=>{
  if(status!==200)return route.fulfill({status,json:{status,detail:'PRIVATE SERVER DETAIL'}});
  if(empty){
   const detail=new URL(route.request().url()).pathname.endsWith(orderId);
   return route.fulfill({json:detail?{...order,lines:[],adjustments:[]}:{items:[]},headers:detail?{ETag:'"1"'}:{}});
  }
  return route.fallback();
 });
 for(const detail of [false,true]){
  const path=`${origin}/t/${tenant}/orders${detail?`/${orderId}`:''}`,name=detail?'order':'orders';
  const pending=await holdResponse(page,`**/api/v1/t/*/orders${detail?`/${orderId}`:'?*'}`);
  await page.goto(path);await pending.ready;
  try{
   if(detail)await expect(page.getByRole('status')).toHaveText('Loading order…');
   else await expect(page.getByRole('table',{name:'Order list'})).toHaveAttribute('aria-busy','true');
   await audit(page,`admin-production-${name}-loading.png`);
  }finally{pending.release();}
  await expect(page.getByRole(detail?'heading':'link',{name:detail?'Order 1001':'1001',exact:true})).toBeVisible();
  await expect(page.getByText(/CONTRACT FIXTURE/)).toHaveCount(0);
  await audit(page,`admin-production-${name}.png`);
  empty=true;await page.reload();
  if(detail){await expect(page.getByRole('table',{name:'Order lines'})).toHaveAttribute('aria-busy','false');await expect(page.getByRole('table',{name:'Order lines'}).getByRole('row')).toHaveCount(2);}
  else await expect(page.getByText('No orders match this filter.',{exact:true})).toBeVisible();
  await audit(page,`admin-production-${name}-empty.png`);
  status=503;await page.reload();
  await expect(page.getByRole('alert')).toContainText('temporarily unavailable');
  await expect(page.getByText('PRIVATE SERVER DETAIL')).toHaveCount(0);
  await audit(page,`admin-production-${name}-error.png`);
  status=200;empty=false;await page.getByRole('button',{name:'Retry',exact:true}).click();
  await expect(page.getByRole(detail?'heading':'link',{name:detail?'Order 1001':'1001',exact:true})).toBeVisible();
 }
});

test('production partial and full refunds preserve read-only totals and long order content',async({page})=>{
 await fixture(page);let state='partially_refunded';
 const sku='LONG-SKU-WITH-MULTILINGUAL-CONTENT-商品-'.repeat(5);
 await page.route('**/api/v1/t/*/orders**',route=>{
  const detail=new URL(route.request().url()).pathname.endsWith(orderId);
  const value={...order,state,total_minor:'5780',tax_total_minor:'480',shipping:{...order.shipping,amount_minor:'500'},lines:[{...order.lines[0],sku_code:sku,quantity:2,unit_price_minor:'2400',tax_minor:'480',line_total_minor:'5280'}],adjustments:[]};
  return route.fulfill({json:detail?value:{items:[{...value,customer_kind:'guest'}]},headers:detail?{ETag:'"1"'}:{}});
 });
 for(state of ['partially_refunded','refunded']){
  await page.goto(`${origin}/t/${tenant}/orders`);
  await expect(page.getByRole('cell',{name:state.replaceAll('_',' '),exact:true})).toBeVisible();
  await audit(page,`admin-production-orders-${state.replaceAll('_','-')}.png`);
  await page.getByRole('link',{name:'1001',exact:true}).click();
  await expect(page.getByRole('heading',{name:'Order 1001',exact:true})).toBeVisible();
  await expect(page.getByText(state.replaceAll('_',' '),{exact:true})).toBeVisible();
  await expect(page.getByRole('cell',{name:sku,exact:true})).toBeVisible();
  await expect(page.getByText('USD 57.80',{exact:true})).toBeVisible();
  await expect(page.getByRole('button',{name:/refund|ship/i})).toHaveCount(0);
  await expect(page.getByText(/CONTRACT FIXTURE/)).toHaveCount(0);
  await audit(page,`admin-production-order-${state.replaceAll('_','-')}-long.png`);
 }
});

test('production publication access has deterministic loading, retry and low-permission states',async({page})=>{
 await fixture(page);let status=503;
 await page.route('**/api/v1/t/*/price-lists',route=>route.fulfill({status,json:{status,detail:'PRIVATE SERVER DETAIL'}}));
 const pending=await holdResponse(page,'**/api/v1/t/*/price-lists');
 await page.goto(`${origin}/t/${tenant}/products/${productId}`);await pending.ready;
 try{
  await expect(page.getByRole('status').filter({hasText:'Checking catalog editing access'})).toBeVisible();
  await audit(page,'admin-publication-loading.png');
 }finally{pending.release();}
 await expect(page.getByRole('alert')).toContainText('Unable to check catalog editing access.');
 await expect(page.getByRole('button',{name:'Load content',exact:true})).toHaveCount(0);
 await audit(page,'admin-publication-access-error.png');
 status=403;await page.getByRole('button',{name:'Retry editing access',exact:true}).click();
 await expect(page.getByRole('status').filter({hasText:'Catalog editing is unavailable for this session.'})).toBeVisible();
 await page.getByLabel('SKU code',{exact:true}).fill('LOW-PERMISSION-SKU');
 await page.getByRole('button',{name:'Create SKU',exact:true}).click();
 await page.getByRole('button',{name:'Edit LOW-PERMISSION-SKU',exact:true}).click();
 await expect(page.getByRole('heading',{name:'Variant price',exact:true})).toHaveCount(0);
 await expect(page.getByRole('button',{name:'Load content',exact:true})).toHaveCount(0);
 await expect(page.getByRole('button',{name:'Save price',exact:true})).toHaveCount(0);
 await expect(page.getByText('PRIVATE SERVER DETAIL')).toHaveCount(0);
 await audit(page,'admin-publication-forbidden.png');
});
