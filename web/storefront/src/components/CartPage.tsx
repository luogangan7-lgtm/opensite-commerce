/** Responsibility: versioned cart review and line mutations.
 * Dependencies: React/shared UI, same-origin commerce client; caller: cart SSR route.
 * Invariants: sends server ETag; 412 refreshes without retrying or overwriting another tab; API owns pricing.
 */
import { useEffect, useState } from 'react';
import { Button, EmptyState, FormField, TextInput } from '@opensite/ui';
import { CommerceError, getCart, setCartLine, removeCartLine } from '../lib/commerce-client';
import type { CartEnvelope, CartLine, PurchaseContext } from '../lib/commerce-contract';
import { commerceCopy, type CommerceDictionary } from '../lib/commerce-copy';
import { formatAmount } from '../lib/presentation';
import { PurchaseNotice } from './PurchaseShared';
function Line({ line, currency, locale, busy, ui, change }: { line: CartLine; currency: string; locale: string; busy: boolean; ui: CommerceDictionary; change: (sku: string, quantity?: number) => void }) {
  const [quantity, setQuantity] = useState(String(line.quantity));
  useEffect(() => { setQuantity(String(line.quantity)); }, [line.quantity]);
  const valid = /^\d+$/.test(quantity) && Number.isSafeInteger(Number(quantity)) && Number(quantity) >= 1 && Number(quantity) <= 999;
  return <li className="cart-line" data-cart-line={line.sku_id}>
    <div className="cart-line-heading"><div><h2>{line.name}</h2><p className="purchase-muted">{line.sku_code}</p></div>
      <p className="product-price">{line.line_amount === undefined ? ui.pricingUnavailable : formatAmount(line.line_amount, currency, locale)}</p>
    </div>
    {!line.sellable && <p className="purchase-error">{ui.lineUnavailable}</p>}
    <div className="purchase-actions">
      <FormField id={`quantity-${line.sku_id}`} label={ui.quantity}><TextInput type="number" min={1} max={999} step={1} value={quantity} disabled={busy} onChange={event => setQuantity(event.target.value)} /></FormField>
      <Button variant="secondary" disabled={busy || !valid || Number(quantity) === line.quantity} onClick={() => change(line.sku_id, Number(quantity))}>{ui.updateQuantity}</Button>
      <Button variant="ghost" disabled={busy} onClick={() => change(line.sku_id)}>{ui.remove}</Button>
    </div>
  </li>;
}
export default function CartPage({ context, locale = 'en' }: { context: PurchaseContext; locale?: string }) {
  const ui = commerceCopy(locale);
  const [envelope, setEnvelope] = useState<CartEnvelope | null>(null);
  const [loading, setLoading] = useState(context.enabled);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [hint, setHint] = useState('');
  async function load() {
    setLoading(true); setError('');
    try { setEnvelope(await getCart()); } catch { setError(ui.requestFailed); }
    finally { setLoading(false); }
  }
  useEffect(() => { if (context.enabled) void load(); }, []);
  async function change(skuID: string, quantity?: number) {
    if (!envelope || busy || !context.enabled) return;
    setBusy(true); setError(''); setHint('');
    try {
      const updated = quantity === undefined ? await removeCartLine(skuID, envelope.etag, context.csrf) : await setCartLine(skuID, quantity, envelope.etag, context.csrf);
      setEnvelope(updated);
    } catch (cause) {
      if (cause instanceof CommerceError && cause.status === 412) {
        try { setEnvelope(await getCart()); setHint(ui.cartChanged); } catch { setError(ui.requestFailed); }
      } else setError(ui.requestFailed);
    } finally { setBusy(false); }
  }
  const cart = envelope?.cart;
  const ready = !!cart?.lines.length && cart.subtotal !== undefined && !cart.pricing_error && cart.lines.every(line => line.sellable && !line.pricing_error);
  return <div data-cart-ready="true" className="purchase-page" aria-busy={loading || busy}>
    <PurchaseNotice context={context} locale={locale} />
    {context.enabled && <>
      {loading && <p role="status">{ui.loading}</p>}
      {error && <div className="purchase-error" role="alert"><p>{error}</p>{!cart && <Button variant="secondary" onClick={load}>{ui.retry}</Button>}</div>}
      {hint && <p className="purchase-notice" role="status">{hint}</p>}
      {!loading && !error && !cart?.lines.length && <EmptyState title={ui.emptyCart} description={ui.emptyCartDescription} action={<a href="/products">{ui.browseProducts}</a>} />}
      {cart && cart.lines.length > 0 && <div className="purchase-layout">
        <ul className="cart-lines">{cart.lines.map(line => <Line key={line.sku_id} line={line} currency={cart.currency} locale={locale} busy={busy} ui={ui} change={change} />)}</ul>
        <aside className="purchase-summary"><dl className="purchase-totals"><div><dt>{ui.subtotal}</dt><dd>{cart.subtotal === undefined ? ui.pricingUnavailable : formatAmount(cart.subtotal, cart.currency, locale)}</dd></div></dl>
          {ready && !busy ? <a className="purchase-primary-link" href="/checkout">{ui.continueCheckout}</a> : <Button disabled>{ui.continueCheckout}</Button>}
        </aside>
      </div>}
    </>}
  </div>;
}
