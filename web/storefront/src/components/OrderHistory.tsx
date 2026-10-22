/** Responsibility: customer history list/detail through the same-origin BFF.
 * Dependencies: shared UI, exact money formatter and purchase transport; caller: account/orders.
 * Invariants: no credential in browser state; no cached history after sign-out; readable loading/error/empty states.
 */
import { useEffect, useState } from 'react';
import { Button } from '@opensite/ui';
import { purchaseRequest, CommerceError } from '../lib/commerce-client';
import { formatAmount } from '../lib/presentation';
type Order={id:string;order_number:string;state:string;currency:string;total_minor:string;created_at:string};
type Detail={order:Order;shipping_minor:string;tax_minor:string;lines:{line_id:string;sku_code:string;quantity:string;line_total_minor:string}[]};
type Orders={items:Order[];next_cursor?:string};
export default function OrderHistory(){
 const [page,setPage]=useState<Orders>(),[detail,setDetail]=useState<Detail>(),[error,setError]=useState(''),[busy,setBusy]=useState(true);
 async function load(id?:string,cursor?:string){
  setBusy(true);setError('');setDetail(undefined);setPage(undefined);
  try{if(id)setDetail(await purchaseRequest<Detail>(`history/${id}`));else setPage(await purchaseRequest<Orders>(`history${cursor?`?cursor=${encodeURIComponent(cursor)}`:''}`));}
  catch(e){setError(e instanceof CommerceError&&e.status===401?'Sign in to view your orders.':e instanceof CommerceError&&e.status===404?'Order not found.':'Orders could not be loaded. Try again.');}
  finally{setBusy(false);}
 }
 useEffect(()=>{void load();},[]);
 return <div className="purchase-page" aria-busy={busy}>
  {busy&&<p role="status">Loading orders…</p>}
  {error&&<><p role="alert">{error}</p><div className="purchase-actions"><a href="/account">Your account</a><Button variant="secondary" onClick={()=>void load()}>Try again</Button></div></>}
  {page&&<>{page.items.length===0?<p>No orders yet. <a href="/products">Browse products</a></p>:<ul className="cart-lines">{page.items.map(o=><li key={o.id} className="cart-line"><div className="cart-line-heading"><h2><Button variant="secondary" onClick={()=>void load(o.id)}>Order #{o.order_number}</Button></h2><p>{formatAmount(o.total_minor,o.currency,'en')}</p></div><p>{o.state.replaceAll('_',' ')} · <time dateTime={o.created_at}>{new Date(o.created_at).toLocaleDateString('en-US',{timeZone:'UTC'})}</time></p></li>)}</ul>}{page.next_cursor&&<Button variant="secondary" onClick={()=>void load(undefined,page.next_cursor)}>Next orders</Button>}</>}
  {detail&&<section aria-label="Order details"><h2>Order #{detail.order.order_number}</h2><p>{detail.order.state.replaceAll('_',' ')}</p><ul className="cart-lines">{detail.lines.map(l=><li key={l.line_id} className="cart-line">{l.sku_code} · Quantity {l.quantity}<p>{formatAmount(l.line_total_minor,detail.order.currency,'en')}</p></li>)}</ul><dl className="purchase-totals">{(['shipping_minor','tax_minor'] as const).map(k=><div key={k}><dt>{k==='shipping_minor'?'Shipping':'Tax'}</dt><dd>{formatAmount(detail[k],detail.order.currency,'en')}</dd></div>)}<div className="purchase-total"><dt>Total</dt><dd>{formatAmount(detail.order.total_minor,detail.order.currency,'en')}</dd></div></dl><Button variant="secondary" onClick={()=>void load()}>Back to orders</Button></section>}
 </div>;
}
