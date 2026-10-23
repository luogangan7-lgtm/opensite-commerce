/** Admin recovery UI states; intercepted DTOs prove UI behavior only, not delivery. */
import {expect,test} from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import {fixture,tenant} from './admin-fixture';
const origin=process.env.OPENSITE_ADMIN_E2E_ORIGIN??'http://127.0.0.1:4174';
const path=`${origin}/t/${tenant}/notifications`;
const id='c3400001-0000-4000-8000-000000000001';
const failure={id,event_id:id,template_key:'order.shipped',status:'dead_letter',attempts:10,generation:1,created_at:'2026-01-01T00:00:00Z',updated_at:'2026-01-01T00:00:00Z',safe_error:'NOTIFICATION_SEND_FAILED'};
test('notification snapshot pages and confirmation retain safe recovery states',async({page})=>{
 await fixture(page);let status=503;const keys:string[]=[];
 const rows=Array.from({length:12},(_,i)=>({...failure,id:`c3400001-0000-4000-8000-${String(i+1).padStart(12,'0')}`,status:i===1?'uncertain':'dead_letter'}));
 await page.route('**/notifications/**',async route=>{
  if(route.request().method()==='GET')return route.fulfill({json:{items:rows}});
  keys.push(route.request().headers()['idempotency-key']);
  return route.fulfill({status,contentType:'application/problem+json',json:{code:'PRIVATE_CONTACT_DETAIL',detail:'PRIVATE_CONTACT_DETAIL'}});
 });
 await page.goto(path);
 await expect(page.getByRole('table',{name:'Notification failures'}).getByRole('row')).toHaveCount(11);
 await expect(page.getByText('Investigate manually',{exact:true})).toBeVisible();
 await page.getByRole('button',{name:'Next page',exact:true}).click();
 await expect(page.getByRole('table',{name:'Notification failures'}).getByRole('row')).toHaveCount(3);
 await page.getByRole('button',{name:'Previous page',exact:true}).click();
 expect((await new AxeBuilder({page}).analyze()).violations).toEqual([]);
 if(process.env.OPENSITE_ADMIN_NO_SNAPSHOTS!=='1')await expect(page).toHaveScreenshot('admin-notifications.png',{fullPage:true});
 const trigger=page.getByRole('row').filter({hasText:id}).getByRole('button',{name:'Redrive',exact:true});
 await trigger.focus();await page.keyboard.press('Enter');
 const dialog=page.getByRole('dialog',{name:'Redrive notification?'});
 await expect(dialog.getByRole('button',{name:'Cancel',exact:true})).toBeFocused();
 await page.keyboard.press('Escape');await expect(trigger).toBeFocused();expect(keys).toEqual([]);
 await trigger.click();await dialog.getByRole('button',{name:'Confirm redrive',exact:true}).click();
 await expect(dialog.getByRole('alert')).toContainText('Retry uses the same request key');
 status=409;await dialog.getByRole('button',{name:'Confirm redrive',exact:true}).click();
 await expect(page.getByRole('status').filter({hasText:'state changed'})).toBeVisible();
 expect(keys).toHaveLength(2);expect(keys[0]).toBe(keys[1]);
 await expect(trigger).toBeDisabled();await expect(page.getByText('PRIVATE_CONTACT_DETAIL')).toHaveCount(0);
 await page.getByRole('button',{name:'Reload failures',exact:true}).click();await expect(trigger).toBeEnabled();
});
test('notification 403 hides all private metadata and mutation controls',async({page})=>{
 await fixture(page);
 await page.route('**/notifications/**',route=>route.fulfill({status:403,json:{detail:'PRIVATE_CONTACT_DETAIL'}}));
 await page.goto(path);
 await expect(page.getByText('Notification recovery is unavailable for this session.',{exact:true})).toBeVisible();
 await expect(page.getByRole('button',{name:'Redrive',exact:true})).toHaveCount(0);
 await expect(page.getByRole('table')).toHaveCount(0);
 await expect(page.getByText('PRIVATE_CONTACT_DETAIL')).toHaveCount(0);
});
