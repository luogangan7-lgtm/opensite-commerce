/** Responsibility: server-only customer session validation and cookie custody.
 * Dependencies: generated commerce transport, Node crypto, Astro cookies; caller: purchasing BFF.
 * Invariants: DB owns identity/liveness; exact pinned Origin and customer CSRF; no bearer in JSON or guest downgrade.
 */
import { createHmac, timingSafeEqual } from 'node:crypto';
import type { AstroCookies } from 'astro';
import { backendRequest } from './commerce-backend';
import { readAPIOrigin } from './presentation';
type Jar=Pick<AstroCookies,'get'|'set'>;
export type CustomerIdentity={id:string;headers:Record<string,string>};
export class CustomerError extends Error {
  constructor(readonly status:number,readonly code='purchase_request_failed'){super(code);}
}
const SESSION='__Host-osc_customer_session', CHALLENGE='__Host-osc_customer_challenge';
export function checkCustomerCookies(request:Request){
  const names=(request.headers.get('Cookie')??'').split(';').map(pair=>pair.trim().split('=')[0]);
  if([SESSION,CHALLENGE].some(name=>names.filter(value=>value===name).length>1))throw new CustomerError(401,'unauthenticated');
}
function validToken(value:string){return /^[A-Za-z0-9_-]{43}$/.test(value)&&Buffer.from(value,'base64url').toString('base64url')===value;}
function customerOrigin(){const site=readAPIOrigin(process.env.OPENSITE_STOREFRONT_ORIGIN??'');if(!site.startsWith('https://'))throw new CustomerError(503);return site;}
function proof(tenant:string,token:string){
  const encoded=process.env.OPENSITE_CUSTOMER_CSRF_SECRET??'',key=Buffer.from(encoded,'base64url');
  if(key.length<32||key.toString('base64url')!==encoded)throw new CustomerError(503,'customer_auth_unavailable');
  return createHmac('sha256',key).update(`customer-http:${customerOrigin()}:customer-session:${tenant}:${token}`).digest('base64url');
}
function cookieHeaders(cookies:Jar){
  const token=cookies.get(SESSION)?.value;
  if(token!==undefined&&(!token||!validToken(token)))throw new CustomerError(401,'unauthenticated');
  const challenge=cookies.get(CHALLENGE)?.value;
  if(!token&&challenge!==undefined&&!/^\d{1,12}\.[A-Za-z0-9_-]{43}\.[A-Za-z0-9_-]{43}$/.test(challenge))throw new CustomerError(401,'unauthenticated');
  return {token,headers:{Origin:customerOrigin(),...((token||challenge)?{Cookie:token?`${SESSION}=${token}`:`${CHALLENGE}=${challenge}`}:{})}};
}
function relay(response:Response,cookies:Jar){
  for(const raw of response.headers.getSetCookie()){
    const [pair,...attributes]=raw.split(';').map(s=>s.trim()),i=pair!.indexOf('='),name=pair!.slice(0,i),value=pair!.slice(i+1);
    if(![SESSION,CHALLENGE].includes(name)||i<0)throw new CustomerError(502);
    const attrs=new Map(attributes.map(s=>{const i=s.indexOf('=');return i<0?[s.toLowerCase(),'']:[s.slice(0,i).toLowerCase(),s.slice(i+1)];}));
    if(!attrs.has('secure')||!attrs.has('httponly')||attrs.get('path')!=='/'||attrs.get('samesite')?.toLowerCase()!=='lax'||attrs.has('domain'))throw new CustomerError(502);
    const clearing=value===''&&(Number(attrs.get('max-age'))<=0);
    if(!clearing&&((name===SESSION&&!validToken(value))||(name===CHALLENGE&&!/^\d{1,12}\.[A-Za-z0-9_-]{43}\.[A-Za-z0-9_-]{43}$/.test(value))))throw new CustomerError(502);
    const expires=attrs.get('expires')?new Date(attrs.get('expires')!):undefined;
    if(expires&&Number.isNaN(expires.getTime()))throw new CustomerError(502);
    cookies.set(name,value,{secure:true,httpOnly:true,path:'/',sameSite:'lax',...(clearing?{maxAge:0}:expires?{expires}:{})});
  }
}
async function call(origin:string,tenant:string,cookies:Jar,path='session',method?:string,input?:unknown,csrf?:string){
  const {headers}=cookieHeaders(cookies);
  const result=await backendRequest(origin,`/api/v1/t/${tenant}/customer/${path}`,undefined,{...(method?{method}:{}),...(input?{body:input}:{}),headers:{...headers,...(csrf?{'X-CSRF-Token':csrf}:{})}});
  // Preserve identity on transient errors; only an authoritative invalid-session read may clear it.
  if(result.response.ok||(!method&&result.response.status===401))relay(result.response,cookies);
  if(!result.response.ok){
    const status=result.response.status;
    throw new CustomerError([429,503].includes(status)?status:method?400:status===401?401:502,method?'customer_auth_failed':status===401?'unauthenticated':'customer_auth_unavailable');
  }
  if(result.response.status===204)return null;
  try{return JSON.parse(result.text??'') as Record<string,unknown>;}catch{throw new CustomerError(502);}
}
export async function readCustomer(origin:string,tenant:string,cookies:Jar):Promise<CustomerIdentity|null>{
  if(cookies.get(SESSION)===undefined)return null;
  const {token}=cookieHeaders(cookies);if(!token)return null;
  const raw=await call(origin,tenant,cookies),customer=raw?.customer as Record<string,unknown>|undefined;
  if(!customer||customer.tenant_id!==tenant||typeof customer.customer_id!=='string'||!/^[a-f0-9-]{36}$/.test(customer.customer_id))throw new CustomerError(502);
  const expected=proof(tenant,token),actual=Buffer.from(String(raw?.csrf_token??'')),wanted=Buffer.from(expected);
  if(actual.length!==wanted.length||!timingSafeEqual(actual,wanted))throw new CustomerError(502);
  return {id:customer.customer_id,headers:{Origin:customerOrigin(),Cookie:`${SESSION}=${token}`,'X-CSRF-Token':expected}};
}
export async function changeCustomer(origin:string,tenant:string,cookies:Jar,path:string,method:string,input?:unknown){
  proof(tenant,cookies.get(SESSION)?.value??'configuration-check');
  const raw=await call(origin,tenant,cookies);
  if(typeof raw?.csrf_token!=='string'||raw.csrf_token.length>256)throw new CustomerError(502);
  await call(origin,tenant,cookies,path,method,input,raw.csrf_token);
}
