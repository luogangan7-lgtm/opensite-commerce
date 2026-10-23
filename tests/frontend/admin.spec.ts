/** Exercises production UI and pending-contract fixtures; Playwright/axe prove UI behavior only. CI alone writes snapshots. */
import {test,expect,type Page} from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import {fixture,tenant,productId} from './admin-fixture';
const origin=process.env.OPENSITE_ADMIN_E2E_ORIGIN??'http://127.0.0.1:4174';
const fixtureOrigin=process.env.OPENSITE_ADMIN_FIXTURE_ORIGIN??'http://127.0.0.1:4173';
const snapshots=process.env.OPENSITE_ADMIN_NO_SNAPSHOTS!=='1';
async function audit(page:Page,name?:string){await page.evaluate(async()=>{await document.fonts.ready;await Promise.all(document.getAnimations().filter(a=>a.effect?.getComputedTiming().iterations!==Infinity).map(a=>a.finished.catch(()=>{})));});expect((await new AxeBuilder({page}).analyze()).violations).toEqual([]);expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);if(name&&snapshots)await expect(page).toHaveScreenshot(name,{fullPage:true});}
async function signIn(page:Page){await page.getByLabel('Email').fill('staff@example.invalid');await page.getByLabel('Password',{exact:true}).fill(crypto.randomUUID());await page.getByRole('button',{name:'Sign in',exact:true}).click();}
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
 let writes = 0;
 await page.route('**/api/v1/t/*/price-lists**', async route => {
   if (route.request().method() === 'PUT') writes++;
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
 await audit(page);
 await amount.fill('3456');
 await expect(amount).not.toHaveAttribute('aria-invalid', 'true');
 await expect(amount).toHaveAttribute('aria-describedby', 'price-minor-help');
 await expect(page.locator('#price-minor-error')).toHaveCount(0);
 await amount.press('Enter');
 await expect(page.getByRole('status').filter({ hasText: 'Price saved' })).toBeVisible();
 expect(writes).toBe(1);
});

test('content and listing preserve opaque ETags and retain conflicting drafts',async({page})=>{
 await fixture(page);await page.goto(`${origin}/t/${tenant}/products/${productId}`);
 await page.getByRole('button',{name:'Load content',exact:true}).click();
 await expect(page.getByLabel('Title',{exact:true})).toHaveValue('Sample title');
 await page.getByLabel('Title',{exact:true}).fill('Unsaved title');
 await page.getByRole('button',{name:'Save content',exact:true}).click();
 await expect(page.getByRole('button',{name:'Save content',exact:true})).toBeDisabled();
 await expect(page.getByLabel('Title',{exact:true})).toHaveValue('Unsaved title');
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
});
