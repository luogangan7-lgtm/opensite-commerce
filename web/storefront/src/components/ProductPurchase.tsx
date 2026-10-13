/** Responsibility: selectable published SKU purchasing, without replacing the real read renderer.
 * Dependencies: React/native shared controls, same-origin commerce client; caller: PublishedPage.
 * Invariants: SKU comes from explicit context; unavailable production never posts; mutation state is announced.
 */
import { useState } from 'react';
import { Button, FormField, Select, TextInput } from '@opensite/ui';
import { addCartLine, purchaseErrorMessage } from '../lib/commerce-client';
import type { PurchaseContext } from '../lib/commerce-contract';
import { commerceCopy } from '../lib/commerce-copy';
import { formatAmount } from '../lib/presentation';
import { PurchaseNotice } from './PurchaseShared';
export default function ProductPurchase({ context, product_id, locale = 'en' }: { context: PurchaseContext; product_id: string; locale?: string }) {
  const ui = commerceCopy(locale);
  const product = context.products.find(item => item.product_id === product_id);
  const [skuID, setSkuID] = useState(product?.skus.find(sku => sku.available)?.sku_id ?? product?.skus[0]?.sku_id ?? '');
  const [quantity, setQuantity] = useState('1');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [added, setAdded] = useState(false);
  const sku = product?.skus.find(item => item.sku_id === skuID);
  const validQuantity = /^\d+$/.test(quantity) && Number.isSafeInteger(Number(quantity)) && Number(quantity) >= 1 && Number(quantity) <= 999;
  async function add() {
    if (!context.enabled || !sku?.available || !validQuantity || busy) return;
    setBusy(true); setError(''); setAdded(false);
    try { await addCartLine(sku.sku_id, Number(quantity), context.csrf); setAdded(true); }
    catch (cause) { setError(purchaseErrorMessage(cause,ui)); }
    finally { setBusy(false); }
  }
  return <section className="purchase-panel" data-purchase-ready="true">
    <PurchaseNotice context={context} locale={locale} />
    {context.enabled && product && <>
      <div className="purchase-fields">
        <FormField id="purchase-variant" label={ui.variant}><Select value={skuID} onChange={event => { setSkuID(event.target.value); setAdded(false); }}>
          {product.skus.map(item => <option key={item.sku_id} value={item.sku_id} disabled={!item.available}>
            {item.option_values.map(option => `${option.name}: ${option.value}`).join(' / ') || item.sku_code}{!item.available ? ` — ${ui.outOfStock}` : ''}
          </option>)}
        </Select></FormField>
        <FormField id="purchase-quantity" label={ui.quantity}><TextInput type="number" min={1} max={999} step={1} value={quantity} onChange={event => setQuantity(event.target.value)} /></FormField>
      </div>
      {sku && <p className="product-price"><span className="purchase-muted">{ui.selectedPrice}: </span>{formatAmount(sku.price_minor, sku.currency, locale)}</p>}
      <div className="purchase-actions"><Button loading={busy} disabled={!sku?.available || !validQuantity} onClick={add}>{ui.addCart}</Button><a href="/cart">{ui.viewCart}</a></div>
      {error && <p className="purchase-error" role="alert">{error}</p>}
      {added && <p role="status">{ui.added}</p>}
    </>}
  </section>;
}
