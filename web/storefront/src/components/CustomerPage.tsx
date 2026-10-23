/** Responsibility: customer session/registration UI backed by server-validated identity.
 * Dependencies: React/shared fields, same-origin customer client; caller: account SSR route.
 * Invariants: registration remains verification-pending; refresh validated actor/CSRF after session transitions; no browser bearer.
 */
import { useState, type SubmitEvent } from 'react';
import { Button, FormField, TextInput } from '@opensite/ui';
import { customerSession, registerCustomer, logoutCustomer, getPurchaseContext } from '../lib/commerce-client';
import type { PurchaseContext } from '../lib/commerce-contract';
import { commerceCopy } from '../lib/commerce-copy';
import { PurchaseNotice } from './PurchaseShared';
export default function CustomerPage({ context: initialContext, locale = 'en' }: { context: PurchaseContext; locale?: string }) {
  const ui = commerceCopy(locale,initialContext.fixture);
  const [context, setContext] = useState(initialContext);
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [emailError, setEmailError] = useState('');
  const [passwordError, setPasswordError] = useState('');
  const [hint, setHint] = useState('');
  async function authenticate(action: 'session' | 'register') {
    if (!context.enabled || busy) return;
    const badEmail = email.length > 254 || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email.trim());
    const bytes=new TextEncoder().encode(password).byteLength;
    const badPassword = context.fixture ? password.length<8||password.length>128 : bytes<(action==='register'?12:1)||bytes>1024||/[\0\r\n]/.test(password);
    setEmailError(badEmail ? ui.invalidEmail : ''); setPasswordError(badPassword ? ui.invalidPassword : '');
    if (badEmail || badPassword) return;
    setBusy(true); setError(''); setHint('');
    try {
      if (action === 'register') { await registerCustomer(email.trim(), password, context.csrf); setHint(ui.registered); }
      else { await customerSession(email.trim(), password, context.csrf); setContext(await getPurchaseContext()); setHint(ui.signedIn); }
      setPassword('');
    } catch { setError(ui.requestFailed); }
    finally { setBusy(false); }
  }
  function signIn(event: SubmitEvent<HTMLFormElement>) { event.preventDefault(); void authenticate('session'); }
  async function logout() {
    if (busy || !context.enabled) return;
    setBusy(true); setError(''); setHint('');
    try { await logoutCustomer(context.csrf); setContext(await getPurchaseContext()); setHint(ui.signedOut); }
    catch { setError(ui.requestFailed); }
    finally { setBusy(false); }
  }
  return <div className="purchase-page customer-page" data-customer-ready="true" aria-busy={busy}>
    <PurchaseNotice context={context} locale={locale} />
    {context.enabled && <>
      <p className="purchase-muted">{ui.accountHelp}</p>
      {context.actor === 'customer' ? <><p>{ui.signedIn}</p><div className="purchase-actions"><Button variant="secondary" loading={busy} onClick={logout}>{ui.signOut}</Button><a href="/cart">{ui.viewCart}</a><a href="/account/orders">{ui.orderHistory}</a></div></> : <>
        <p>{ui.guestSession}</p><form onSubmit={signIn} noValidate className="customer-form">
          <FormField id="customer-email" label={ui.email} error={emailError}><TextInput type="email" autoComplete="email" maxLength={254} value={email} disabled={busy} required onChange={event => { setEmail(event.target.value); setEmailError(''); }} /></FormField>
          <FormField id="customer-password" label={ui.password} help={ui.passwordHelp} error={passwordError}><TextInput type="password" autoComplete="current-password" minLength={context.fixture?8:1} maxLength={context.fixture?128:1024} value={password} disabled={busy} required onChange={event => { setPassword(event.target.value); setPasswordError(''); }} /></FormField>
          <div className="purchase-actions"><Button type="submit" loading={busy}>{ui.signIn}</Button><Button variant="secondary" disabled={busy} onClick={() => void authenticate('register')}>{ui.register}</Button></div>
        </form>
      </>}
      {hint && <p className="purchase-notice" role="status">{hint}</p>}
      {error && <p className="purchase-error" role="alert">{error}</p>}
    </>}
  </div>;
}
