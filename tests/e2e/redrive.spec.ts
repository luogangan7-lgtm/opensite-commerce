/** CI real notification recovery: production UI/HTTP, actual SMTP and Mailpit.
 * Dependencies: owned final-attempt source fixtures in run.py; no intercepted routes.
 * Invariants: one delivery after replay, scope denial and cross-tenant isolation. */
import {expect,test} from '@playwright/test';
import {audit,fixture} from './fixture';

test('real admin notification failure redrive delivers once with scoped isolation',async({page},info)=>{
 const suffix=info.project.name==='desktop'?'000000000001':'000000000002';
 const tenant=`c1900010-0000-4000-8000-${suffix}`, event=`c3400001-0000-4000-8000-${suffix}`;
 const base=`${fixture.admin}/api/v1/t/${tenant}/notifications`;
 await page.goto(`${fixture.admin}/t/${tenant}/notifications`);
 await page.getByLabel('Email',{exact:true}).fill(fixture.ownerEmail);
 await page.getByLabel('Password',{exact:true}).fill(fixture.ownerPassword);
 await page.getByRole('button',{name:'Sign in',exact:true}).click();
 await expect(page.getByRole('heading',{name:'Notification failures',exact:true})).toBeVisible();
 const response=await page.request.get(`${base}/failures?limit=100`);
 expect(response.status()).toBe(200);
 const failed=(await response.json()).items.find((item:{event_id:string})=>item.event_id===event);
 expect(failed).toMatchObject({status:'dead_letter',safe_error:'NOTIFICATION_SEND_FAILED',generation:1});
 expect(failed).not.toHaveProperty('recipient');
 const row=page.getByRole('row').filter({hasText:failed.id});
 await expect(row).toContainText('Delivery failed');
 await audit(page,info);
 await row.getByRole('button',{name:'Redrive',exact:true}).focus();
 await page.keyboard.press('Enter');
 const dialog=page.getByRole('dialog',{name:'Redrive notification?'});
 await expect(dialog).toBeVisible();
 await dialog.getByRole('button',{name:'Cancel redrive',exact:true}).click();
 await expect(row.getByRole('button',{name:'Redrive',exact:true})).toBeFocused();
 await row.getByRole('button',{name:'Redrive',exact:true}).click();
 const redriven=page.waitForResponse(r=>r.url()===`${base}/${failed.id}/redrive`&&r.request().method()==='POST');
 await dialog.getByRole('button',{name:'Confirm redrive',exact:true}).click();
 const receiptResponse=await redriven,receipt=await receiptResponse.json();
 expect(receiptResponse.status()).toBe(200);expect(receipt.generation).toBe(2);
 await expect(page.getByRole('status').filter({hasText:'Redrive queued'})).toBeVisible();
 const session=await(await page.request.get(`${fixture.admin}/api/v1/auth/session`)).json();
 const headers={Origin:fixture.admin,'X-CSRF-Token':session.csrf_token,'Idempotency-Key':receiptResponse.request().headers()['idempotency-key']};
 // Mailpit stores real MIME. Match the frozen order reference and shipped subject,
 // excluding other purchase/verification notifications in this shared CI inbox.
 let delivered:string[]=[];
 await expect.poll(async()=>{
  const listing=await(await page.request.get('http://127.0.0.1:8025/api/v1/messages?limit=100')).json();
  delivered=[];
  for(const item of listing.messages.filter((m:{Subject:string})=>m.Subject==='Order shipped')){
   const message=await(await page.request.get(`http://127.0.0.1:8025/api/v1/message/${item.ID}`)).json();
   if(message.Text.includes(`c1900003-0000-4000-8000-${suffix}`))delivered.push(item.ID);
  }
  return delivered.length;
 },{timeout:30_000}).toBe(1);
 // Replay AFTER actual SMTP acceptance so resetting a sent notification is detected.
 const replay=await page.request.post(`${base}/${failed.id}/redrive`,{headers});
 expect(replay.status()).toBe(200);expect(await replay.json()).toEqual(receipt);
 expect((await page.request.post(`${base}/${failed.id}/redrive`,{headers:{...headers,'Idempotency-Key':crypto.randomUUID()}})).status()).toBe(409);
 expect((await page.request.post(`${fixture.admin}/api/v1/t/${fixture.ownerTenantId}/notifications/${failed.id}/redrive`,{headers})).status()).toBe(404);
 await info.attach('notification-redrive',{body:JSON.stringify({event,notification:failed.id,receipt,mailpit:delivered}),contentType:'application/json'});
 await page.getByRole('button',{name:'Reload failures',exact:true}).click();
 await expect(page.getByRole('row').filter({hasText:failed.id})).toHaveCount(0);
 await page.getByRole('button',{name:'Sign out',exact:true}).click();
 await page.getByLabel('Email',{exact:true}).fill('staff-viewer@example.test');
 await page.getByLabel('Password',{exact:true}).fill(fixture.ownerPassword);
 await page.getByRole('button',{name:'Sign in',exact:true}).click();
 await page.goto(`${fixture.admin}/t/${tenant}/notifications`);
 await expect(page.getByText('Notification recovery is unavailable for this session.',{exact:true})).toBeVisible();
 await expect(page.getByRole('button',{name:'Redrive',exact:true})).toHaveCount(0);
 const viewer=await(await page.request.get(`${fixture.admin}/api/v1/auth/session`)).json();
 expect((await page.request.post(`${base}/${failed.id}/redrive`,{headers:{...headers,'X-CSRF-Token':viewer.csrf_token,'Idempotency-Key':crypto.randomUUID()}})).status()).toBe(403);
 await audit(page,info);
});
