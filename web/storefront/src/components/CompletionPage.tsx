/** Responsibility: captured-only order receipt.
 * Dependencies: React/shared UI, same-origin completion client and exact money formatter; caller: completion SSR route.
 * Invariants: URL/query parameters cannot create confirmation; only service-returned captured payment qualifies.
 */
import { useEffect, useState } from 'react';
import { Button, EmptyState } from '@opensite/ui';
import { getCompletion } from '../lib/commerce-client';
import type { CheckoutResult, PurchaseContext } from '../lib/commerce-contract';
import { commerceCopy } from '../lib/commerce-copy';
import { formatAmount } from '../lib/presentation';
import { PurchaseNotice } from './PurchaseShared';
export default function CompletionPage({ context, locale = 'en' }: { context: PurchaseContext; locale?: string }) {
  const ui = commerceCopy(locale);
  const [result, setResult] = useState<CheckoutResult | null>(null);
  const [loading, setLoading] = useState(context.enabled);
  const [error, setError] = useState('');
  async function load() {
    setLoading(true); setError('');
    try { setResult(await getCompletion()); } catch { setError(ui.requestFailed); }
    finally { setLoading(false); }
  }
  useEffect(() => { if (context.enabled) void load(); }, []);
  return <div className="purchase-page" data-completion-page="true" aria-busy={loading}>
    <PurchaseNotice context={context} locale={locale} />
    {context.enabled && <>
      {loading && <p role="status">{ui.loading}</p>}
      {error && <div className="purchase-error" role="alert"><p>{error}</p><Button variant="secondary" onClick={load}>{ui.retry}</Button></div>}
      {!loading && !error && (result?.payment.status === 'captured' ? <section className="payment-result" data-completion-ready="true" data-payment-status="captured">
        <h2>{ui.paymentCaptured}</h2><dl className="purchase-totals"><div><dt>{ui.orderId}</dt><dd>{result.order_id}</dd></div><div><dt>{ui.total}</dt><dd>{formatAmount(result.amount_minor, result.currency, locale)}</dd></div><div><dt>{ui.paymentStatus}</dt><dd>{ui.paymentCaptured}</dd></div></dl>
        <a href="/products">{ui.returnProducts}</a>
      </section> : <EmptyState title={ui.noCompletion} description={ui.noCompletionDescription} action={<a href="/cart">{ui.backCart}</a>} />)}
    </>}
  </div>;
}
