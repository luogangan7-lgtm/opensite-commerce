/** Notification transport admission; CI executes against cookie transport, no external mail. */
import {expect, test} from 'bun:test';
import {AdminAPI} from './api';
import {NotificationsAPI, parseFailures} from './notifications';
const tenant='c1900010-0000-4000-8000-000000000001', id='c1900003-0000-4000-8000-000000000001';
const failure={id,event_id:id,template_key:'order.shipped',status:'dead_letter',attempts:1,generation:1,created_at:'2026-10-24T00:00:00Z',updated_at:'2026-10-24T00:00:00Z',safe_error:'NOTIFICATION_SEND_FAILED'} as const;
test('notification metadata is bounded, projected and contains no contact data',()=>{
 expect(parseFailures({items:[{...failure,recipient:'private@example.test',subject:'private'}]})).toEqual([failure]);
 for(const value of [{items:Array(101).fill(failure)},{items:[{...failure,status:'sent'}]},{items:[{...failure,attempts:-1}]},{items:[{...failure,safe_error:'private@example.test'}]},{items:[{...failure,generation:1.5}]}]) expect(()=>parseFailures(value)).toThrow();
});
test('redrive retries keep the explicit key, no body, CSRF and no-store',async()=>{
 const calls:RequestInit[]=[];let count=0;
 const api=new AdminAPI({fetch:(async(_url,init)=>{
  calls.push(init??{}); count++;
  if(count===1)return Response.json({user_id:id,session_id:id,expires_at:'2099-01-01T00:00:00Z',csrf_token:'csrf'});
  if(count===2)throw new TypeError('lost response');
  return Response.json({notification_id:id,token:id,status:'pending',generation:2});
 }) as typeof fetch});
 await api.session();const notifications=new NotificationsAPI(api,tenant);
 await expect(notifications.redrive(id,'stable-request')).rejects.toThrow();
 expect(await notifications.redrive(id,'stable-request')).toMatchObject({notification_id:id,generation:2});
 for(const call of calls.slice(1)){
  expect(call.method).toBe('POST');expect(call.body).toBeUndefined();expect(call.cache).toBe('no-store');
  expect(new Headers(call.headers).get('Idempotency-Key')).toBe('stable-request');
  expect(new Headers(call.headers).get('X-CSRF-Token')).toBe('csrf');
 }
});
