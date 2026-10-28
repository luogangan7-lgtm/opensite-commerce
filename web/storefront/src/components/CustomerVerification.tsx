/** Responsibility: consume a mailbox proof from a fragment and POST it once on explicit action.
 * Dependencies: browser-only fragment, same-origin BFF and shared UI; caller: customer/verify SSR.
 * Invariants: clear URL before IO, never persist/render proof, backend pins tenant and one-use semantics.
 */
import { useEffect, useState } from 'react';
import { Button } from '@opensite/ui';
import { commerceCopy } from '../lib/commerce-copy';
import { getPurchaseContext, verifyCustomerEmail } from '../lib/commerce-client';
export default function CustomerVerification({locale='en'}:{locale?:string}){
  const ui=commerceCopy(locale,false);
  const [token,setToken]=useState(''),[busy,setBusy]=useState(false),[message,setMessage]=useState(ui.verificationOpen);
  useEffect(()=>{
    const fragment=new URLSearchParams(window.location.hash.slice(1));
    window.history.replaceState(null,'',window.location.pathname);
    const proof=fragment.get('token')??'';
    if(fragment.getAll('token').length===1&&/^[A-Za-z0-9_-]{43}$/.test(proof)){setToken(proof);setMessage(ui.verificationReady);}
  },[]);
  async function verify(){
    if(busy||!token)return;setBusy(true);
    try{const context=await getPurchaseContext();if(!context.enabled||context.fixture)throw new Error('Unavailable');await verifyCustomerEmail(token,context.csrf);setMessage(ui.verificationDone);}
    catch{setMessage(ui.verificationFailed);}
    finally{setToken('');setBusy(false);}
  }
  return <div className="purchase-page" data-customer-verification="true" aria-busy={busy}><p role="status">{message}</p><div className="purchase-actions"><Button disabled={!token||busy} loading={busy} onClick={()=>void verify()}>{ui.verifyEmail}</Button><a href="/account">{ui.customerAccount}</a></div></div>;
}
