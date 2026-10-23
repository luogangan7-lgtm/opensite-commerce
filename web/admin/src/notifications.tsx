/** Scoped notification recovery page; composes incumbent UI/tokens and cookie API.
 * Called by admin routes; DB owns time/state/locks. No contacts or message content
 * enter the view; receipt means queued. Pagination is within the API's 100-row cap. */
import {useEffect,useRef,useState} from 'react';
import {Badge,Button,Card,DataTable,Dialog,EmptyState} from '@opensite/ui';
import type {components} from '../../../packages/api-client/src/schema';
import {AdminAPI,APIError} from './api';
import {validateUUID} from './validation';
type Failure=components['schemas']['NotificationFailure'];
type Receipt=components['schemas']['NotificationRedriveReceipt'];
const templates=['order.paid','order.shipped','order.refund','quote.sent','quote.approved','quote.converted'];
function invalid():never{throw new APIError(502,'invalid_response');}
function record(value:unknown):Record<string,unknown>{return value&&typeof value==='object'&&!Array.isArray(value)?value as Record<string,unknown>:invalid();}
function uuid(value:unknown):string{return typeof value==='string'&&validateUUID(value)?value:invalid();}
function integer(value:unknown,min:number):number{return typeof value==='number'&&Number.isInteger(value)&&value>=min&&value<=2147483647?value:invalid();}
function date(value:unknown):string{return typeof value==='string'&&/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?(Z|[+-]\d{2}:\d{2})$/.test(value)&&Number.isFinite(Date.parse(value))?value:invalid();}
export function parseFailures(value:unknown):Failure[]{
 const items=record(value).items;if(!Array.isArray(items)||items.length>1000)return invalid();
 return items.map(value=>{const r=record(value);
  if(!templates.includes(String(r.template_key))||!['failed','dead_letter','uncertain'].includes(String(r.status)))return invalid();
  if(r.safe_error!==undefined&&(typeof r.safe_error!=='string'||!/^[A-Z0-9_]{1,128}$/.test(r.safe_error)))return invalid();
  // Project only admitted metadata even if a future server accidentally adds PII.
  return {id:uuid(r.id),event_id:uuid(r.event_id),template_key:r.template_key as Failure['template_key'],status:r.status as Failure['status'],attempts:integer(r.attempts,0),generation:integer(r.generation,1),created_at:date(r.created_at),updated_at:date(r.updated_at),...(r.safe_error===undefined?{}:{safe_error:r.safe_error})};
 });
}
export class NotificationsAPI{
 private readonly base:string;
 constructor(private readonly api:AdminAPI,tenant:string){this.base=`/api/v1/t/${uuid(tenant)}/notifications`;}
 async list(signal?:AbortSignal):Promise<Failure[]>{return parseFailures(await this.api.request(`${this.base}/failures?limit=100`,'GET',undefined,{signal}));}
 async redrive(id:string,key:string,signal?:AbortSignal):Promise<Receipt>{
  const r=record(await this.api.request(`${this.base}/${uuid(id)}/redrive`,'POST',undefined,{idempotencyKey:key,signal}));
  if(r.notification_id!==id||r.status!=='pending')return invalid();
  return {notification_id:uuid(r.notification_id),token:uuid(r.token),status:'pending',generation:integer(r.generation,2)};
 }
}
const errors:Record<string,string>={NOTIFICATION_SEND_FAILED:'Delivery failed',OUTBOX_ATTEMPT_LIMIT:'Retry limit reached',NOTIFICATION_RECIPIENT_UNAVAILABLE:'Recipient unavailable',NOTIFICATION_SEND_UNCERTAIN:'Delivery outcome unknown',SEND_RESULT_UNKNOWN:'Delivery outcome unknown'};
function redrivable(row:Failure){return row.status==='dead_letter'&&row.safe_error!=='NOTIFICATION_RECIPIENT_UNAVAILABLE';}
export function NotificationFailuresPage({api}:{api:NotificationsAPI}){
 const [rows,setRows]=useState<Failure[]>([]),[page,setPage]=useState(0),[revision,setRevision]=useState(0),[loading,setLoading]=useState(true),[denied,setDenied]=useState(false),[error,setError]=useState(''),[notice,setNotice]=useState(''),[reload,setReload]=useState(false);
 const [selection,setSelection]=useState<{row:Failure;key:string}>(),[busy,setBusy]=useState(false),[mutationError,setMutationError]=useState('');
 const pending=useRef<AbortController|null>(null),mounted=useRef(true);
 useEffect(()=>{mounted.current=true;return()=>{mounted.current=false;pending.current?.abort();};},[]);
 useEffect(()=>{const abort=new AbortController();setLoading(true);setRows([]);setDenied(false);setError('');setPage(0);setSelection(undefined);setReload(false);
  api.list(abort.signal).then(rows=>{if(!abort.signal.aborted)setRows(rows);}).catch(reason=>{if(!abort.signal.aborted){if(reason instanceof APIError&&reason.status===403)setDenied(true);else setError('Unable to load notification failures. Reload to try again.');}}).finally(()=>{if(!abort.signal.aborted)setLoading(false);});
  return()=>abort.abort();
 },[api,revision]);
 async function confirm(){
  if(!selection||pending.current)return;const abort=new AbortController();pending.current=abort;setBusy(true);setMutationError('');
  try{await api.redrive(selection.row.id,selection.key,abort.signal);if(!mounted.current)return;
   setRows(rows=>rows.filter(row=>row.id!==selection.row.id));setSelection(undefined);setNotice('Redrive queued. Delivery has not yet been confirmed.');setReload(true);
  }catch(reason){if(!mounted.current)return;
   if(reason instanceof APIError&&reason.status===403){setDenied(true);setRows([]);setSelection(undefined);}
   else if(reason instanceof APIError&&[404,409,412].includes(reason.status)){setSelection(undefined);setReload(true);setNotice('This notification was already redriven or its state changed. Reload failures before continuing.');}
   else setMutationError('Unable to confirm redrive. Retry uses the same request key.');
  }finally{pending.current=null;if(mounted.current)setBusy(false);}
 }
 const pages=Math.max(1,Math.ceil(rows.length/10)),current=Math.min(page,pages-1);
 return <section className="admin-page" aria-label="Notification recovery"><h1 id="notifications-title">Notification failures</h1>
  <p>Review failed deliveries. Only known dead-letter failures can be redriven; unknown outcomes require investigation.</p>
  <p role="status" aria-live="polite">{notice}</p>
  {denied?<EmptyState headingLevel={2} title="No access" description="Notification recovery is unavailable for this session."/>:<>
   <Button variant="secondary" disabled={busy||loading} onClick={()=>{setNotice('');setRevision(n=>n+1);}}>Reload failures</Button>
   {error?<p className="admin-error" role="alert">{error}</p>:<>
    <p className="admin-muted">Latest {rows.length} failures (maximum 100). Page {current+1} of {pages}. Reload for the latest snapshot.</p>
    <Card><DataTable caption="Notification failures" loading={loading} emptyLabel="No notification failures in this snapshot." columns={[{key:'notification',label:'Notification'},{key:'state',label:'State'},{key:'failure',label:'Failure'},{key:'attempts',label:'Attempts'},{key:'updated',label:'Updated'},{key:'action',label:'Recovery'}]} rows={rows.slice(current*10,current*10+10).map(row=>({id:row.id,notification:<>{row.id}<br/>{row.template_key}</>,state:<Badge tone="neutral">{row.status.replaceAll('_',' ')}</Badge>,failure:errors[row.safe_error??'']??'Delivery requires review',attempts:row.attempts,updated:<time dateTime={row.updated_at}>{row.updated_at}</time>,action:redrivable(row)?<Button disabled={loading||busy||reload} onClick={()=>{setMutationError('');setSelection({row,key:crypto.randomUUID()});}}>Redrive</Button>:row.status==='failed'?'Automatic retry pending':'Investigate manually'}))}/></Card>
    <div className="admin-actions"><Button variant="secondary" disabled={loading||busy||current===0} onClick={()=>setPage(current-1)}>Previous page</Button><Button variant="secondary" disabled={loading||busy||current+1>=pages} onClick={()=>setPage(current+1)}>Next page</Button></div>
   </>}
  </>}
  <Dialog open={!!selection} onOpenChange={open=>{if(!open&&!busy)setSelection(undefined);}} title="Redrive notification?" description="Queue one new delivery attempt. A queued receipt does not confirm email delivery.">
   {selection&&<p>{selection.row.id}</p>}{mutationError&&<p className="admin-error" role="alert">{mutationError}</p>}
   <div className="admin-actions"><Button variant="secondary" disabled={busy} onClick={()=>setSelection(undefined)}>Cancel</Button><Button loading={busy} onClick={confirm}>Confirm redrive</Button></div>
  </Dialog>
 </section>;
}
