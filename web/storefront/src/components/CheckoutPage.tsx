/** Responsibility: validated contact/address, service preview, idempotent checkout and explicit fake confirmation.
 * Dependencies: React/shared controls, checkout validation, same-origin commerce client; caller: checkout SSR route.
 * Invariants: totals invalidated on input edit; stable idempotency key on retry; authorized payment is never completion.
 */
import { useEffect, useRef, useState, type SubmitEvent } from 'react';
import { Button, EmptyState, FormField, TextInput } from '@opensite/ui';
import { getCart, previewCheckout, startCheckout, confirmPayment, purchaseErrorMessage } from '../lib/commerce-client';
import type { CartEnvelope, CheckoutResult, PaymentOutcome, Preview, PurchaseContext } from '../lib/commerce-contract';
import { commerceCopy } from '../lib/commerce-copy';
import { checkoutInput, validateCheckout, type CheckoutErrors, type CheckoutFields } from '../lib/checkout-validation';
import { formatAmount } from '../lib/presentation';
import { PurchaseNotice, PurchaseTotals } from './PurchaseShared';
const initialFields: CheckoutFields = { email: '', name: '', phone: '', country: '', address1: '', address2: '', city: '', subdivision: '', postal_code: '' };
export default function CheckoutPage({ context, locale = 'en' }: { context: PurchaseContext; locale?: string }) {
  const ui = commerceCopy(locale);
  const [cart, setCart] = useState<CartEnvelope | null>(null);
  const [loading, setLoading] = useState(context.enabled);
  const [fields, setFields] = useState(initialFields);
  const [errors, setErrors] = useState<CheckoutErrors>({});
  const [preview, setPreview] = useState<Preview | null>(null);
  const [result, setResult] = useState<CheckoutResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const idempotencyKey = useRef<string | null>(null);
  async function load() {
    setLoading(true); setError('');
    try { setCart(await getCart()); } catch (cause) { setError(purchaseErrorMessage(cause,ui)); }
    finally { setLoading(false); }
  }
  useEffect(() => { if (context.enabled) void load(); }, []);
  function edit(key: keyof CheckoutFields, value: string) {
    setFields(current => ({ ...current, [key]: value })); setPreview(null); idempotencyKey.current = null;
    setErrors(current => ({ ...current, [key]: undefined }));
  }
  async function review(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (busy || !context.enabled) return;
    const invalid = validateCheckout(fields, context.actor); setErrors(invalid);
    if (Object.keys(invalid).length) return;
    setBusy(true); setError(''); setPreview(null);
    try { setPreview(await previewCheckout(checkoutInput(fields, context.actor).shipping_address, context.csrf)); }
    catch (cause) { setError(purchaseErrorMessage(cause,ui)); }
    finally { setBusy(false); }
  }
  async function start() {
    if (!preview || busy || !context.enabled) return;
    const invalid = validateCheckout(fields, context.actor); setErrors(invalid);
    if (Object.keys(invalid).length) return;
    setBusy(true); setError('');
    try {
      if (!idempotencyKey.current) {
        const latest=await previewCheckout(checkoutInput(fields,context.actor).shipping_address,context.csrf);
        if (latest.total_minor !== preview.total_minor || latest.currency !== preview.currency) {
          setPreview(latest);setError(ui.priceChanged);return;
        }
        idempotencyKey.current=crypto.randomUUID();
      }
      const received=await startCheckout(checkoutInput(fields,context.actor),idempotencyKey.current,context.csrf);
      setResult(received);
      if (received.amount_minor !== preview.total_minor || received.currency !== preview.currency) setError(ui.priceChanged);
    }
    catch (cause) { setError(purchaseErrorMessage(cause,ui)); }
    finally { setBusy(false); }
  }
  async function confirm(outcome: PaymentOutcome) {
    if (!(context.fixture || context.fakePayment) || !result || busy) return;
    setBusy(true); setError('');
    try { setResult(await confirmPayment(result.checkout_id, outcome, context.csrf)); }
    catch (cause) { setError(purchaseErrorMessage(cause,ui)); }
    finally { setBusy(false); }
  }
  function input(key: keyof CheckoutFields, required = false, maxLength = 256) {
    const code = errors[key];
    const message = code ? code === 'country' ? ui.invalidCountry : code === 'email' ? ui.invalidEmail : ui[code] : undefined;
    const autocomplete = { email: 'email', name: 'name', phone: 'tel', country: 'country', address1: 'address-line1', address2: 'address-line2', city: 'address-level2', subdivision: 'address-level1', postal_code: 'postal-code' } as const;
    return <FormField id={`checkout-${key}`} label={ui[key]} error={message} help={key === 'country' ? ui.countryHelp : !required ? ui.optional : undefined}>
      <TextInput type={key === 'email' ? 'email' : key === 'phone' ? 'tel' : 'text'} name={key} value={fields[key]} autoComplete={autocomplete[key]} required={required} maxLength={maxLength} disabled={busy} onChange={event => edit(key, event.target.value)} />
    </FormField>;
  }
  const captured = result?.payment.status === 'captured';
  const failed = result?.payment.status === 'failed';
  const canceled = result?.payment.status === 'canceled';
  const ready = !!cart?.cart.lines.length && cart.cart.subtotal !== undefined && !cart.cart.pricing_error && cart.cart.lines.every(line => line.sellable && !line.pricing_error);
  return <div data-checkout-ready="true" className="purchase-page" aria-busy={busy || loading}>
    <PurchaseNotice context={context} locale={locale} />
    {context.enabled && <>
      {loading && <p role="status">{ui.loading}</p>}
      {error && <div className="purchase-error" role="alert"><p>{error}</p>{!cart && <Button variant="secondary" onClick={load}>{ui.retry}</Button>}</div>}
      {!loading && !error && !ready && <EmptyState title={cart?.cart.lines.length ? ui.pricingUnavailable : ui.emptyCart} action={<a href="/cart">{ui.backCart}</a>} />}
      {ready && !result && <div className="purchase-layout">
        <form className="checkout-form" onSubmit={review} noValidate>
          {context.actor === 'guest' && <fieldset disabled={busy}><legend>{ui.checkoutContact}</legend><div className="purchase-fields">{input('email', true, 254)}{input('name', false, 200)}{input('phone', false, 64)}</div></fieldset>}
          <fieldset disabled={busy}><legend>{ui.shippingAddress}</legend><div className="purchase-fields">{input('country', true, 2)}{input('address1', true)}{input('address2')}{input('city', true, 128)}{input('subdivision', false, 100)}{input('postal_code', true, 32)}</div></fieldset>
          <div className="purchase-actions"><Button type="submit" loading={busy}>{ui.reviewTotals}</Button><a href="/cart">{ui.backCart}</a></div>
        </form>
        <aside className="purchase-summary"><h2>{ui.total}</h2><p className="purchase-muted">{ui.totalsDescription}</p>
          {preview && <><PurchaseTotals preview={preview} locale={locale} />
            <h3>{ui.shippingMethods}</h3><ul className="shipping-methods">{preview.shipping_options.map(option => <li key={option.rate_id}>{option.method}</li>)}</ul>
            <Button loading={busy} onClick={start}>{ui.startCheckout}</Button></>}
        </aside>
      </div>}
      {result && <section className="payment-result" data-payment-status={result.payment.status}>
        <p className="product-price">{formatAmount(result.amount_minor,result.currency,locale)}</p>
        <h2>{captured ? ui.paymentCaptured : failed ? ui.paymentFailed : canceled ? ui.paymentCanceled : ui.paymentPending}</h2>
        {captured ? <a className="purchase-primary-link" href="/order-complete">{ui.viewConfirmation}</a> : failed || canceled ? <><p>{ui.paymentRetry}</p><a href="/cart">{ui.backCart}</a></> : <>
          <p>{ui.paymentPendingDescription}</p>
          {(context.fixture || context.fakePayment) && <div className="purchase-actions"><Button loading={busy} onClick={() => confirm('captured')}>{ui.fakeCapture}</Button><Button variant="secondary" disabled={busy} onClick={() => confirm('failed')}>{ui.fakeFail}</Button><Button variant="ghost" disabled={busy} onClick={() => confirm('canceled')}>{ui.fakeCancel}</Button></div>}
        </>}
      </section>}
    </>}
  </div>;
}
