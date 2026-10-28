/** Required real-backend visual evidence; consumed by CI and the browser suite.
 * The issue inventory is explicit: deleting a screenshot call cannot silently pass.
 */
export const REAL_VISUAL_STATES = {
  230: ['list', 'detail', 'long-list', 'long-detail', 'empty', 'pagination',
    'list-loading', 'detail-loading', 'list-503', 'order-detail-unavailable-503',
    'unauthenticated', 'cross-tenant', 'expired-session', 'permission-denied',
    'multiline', 'paid', 'partially-fulfilled', 'fulfilled', 'partially-refunded', 'refunded',
    'rma-cart', 'rma-checkout', 'rma-completion'],
  250: ['account-entry', 'history-list', 'history-detail', 'history-long-order', 'history-long-sku',
    'history-multiline', 'history-loading', 'history-detail-loading', 'history-empty',
    'history-expired-session', 'history-not-found', 'history-list-503', 'history-detail-503',
    'history-pagination', 'history-pagination-loading', 'history-pagination-503',
    'history-focus-restored', 'history-focus-scroll', 'b2b-paid-list', 'b2b-paid-detail'],
  235: ['account-guest', 'account-signed-in', 'account-registration-notice', 'account-validation-errors',
    'account-in-flight', 'account-disabled', 'account-request-failure', 'account-long-email', 'account-long-help',
    'verification-no-proof', 'verification-invalid-proof', 'verification-valid-proof', 'verification-enabled',
    'verification-processing', 'verification-success', 'verification-expired', 'verification-replayed',
    'verification-private', 'account-keyboard-focus', 'verification-keyboard-focus'],
  213: ['checkout-normal', 'checkout-changed-total', 'checkout-confirmed', 'checkout-in-flight',
    'checkout-ambiguous-failure', 'checkout-retry', 'checkout-failed', 'checkout-canceled',
    'completion-paid', 'completion-pending', 'completion-empty', 'completion-loading',
    'completion-error', 'completion-long-order', 'completion-long-amount'],
} as const;

export function realVisualPaths() {
  return Object.entries(REAL_VISUAL_STATES).flatMap(([issue, states]) =>
    states.flatMap(state => ['desktop', 'mobile'].map(viewport =>
      `${viewport}/real-${issue}-${state}.png`)));
}

/** Distinct required history states must never share a committed image. */
export function assertDistinctHistoryPngs(pngs: Record<string, string>) {
  const states = ['history-list', 'history-long-order', 'history-detail', 'history-long-sku', 'history-multiline'];
  for (const viewport of ['desktop', 'mobile']) {
    const hashes = states.map(state => pngs[`${viewport}/real-250-${state}.png`]);
    if (hashes.some(hash => !hash) || new Set(hashes).size !== states.length)
      throw new Error(`${viewport} history state images must exist and be distinct`);
  }
}

export function realBackendEvidence(snapshot: string) {
  const staff = /^real-230-(.+)\.png$/.exec(snapshot.split('/').at(-1) ?? '')?.[1];
  if (staff && ['paid', 'partially-fulfilled', 'fulfilled', 'partially-refunded', 'refunded', 'rma-cart', 'rma-checkout', 'rma-completion'].includes(staff)) return {
    application: 'Go', database: 'PostgreSQL 18.6', mode: 'test',
    seed: 'real published purchase quantity 2; signed test payment; Fulfillment shipment API and Returns receive/refund API; no SQL business writes',
    fault: 'none', boundary: 'real module state and version asserted before each capture; exact runtime UUID/time masks bounds-checked; test PSP only' };
  if (staff) return { application: 'Go', database: 'PostgreSQL 18.6', mode: 'test',
    seed: 'isolated staff tenant and historical sealed Orders executor fixtures; real password login; all DB guards enabled',
    fault: staff.endsWith('-loading') ? 'proxy holds a completed real Go staff orders GET response'
      : staff.endsWith('-503') ? 'proxy substitutes HTTP 503 after forwarding real Go staff orders GET' : 'none',
    boundary: staff === 'expired-session' ? 'owned real staff session expires against DB clock; backend rejects it'
      : staff === 'cross-tenant' ? 'existing second tenant order; real backend tenant predicate returns 404'
      : staff === 'permission-denied' ? 'real viewer membership has no orders:read scope; backend denies admission'
      : 'not-applicable: no admin order write UI (read-only contract #52); malformed read ETag remains contract-tested; safe integer order number, SKU <=255 characters' };

  const history = /^real-250-(.+)\.png$/.exec(snapshot.split('/').at(-1) ?? '')?.[1];
  if (history) return { application: 'Go', database: 'PostgreSQL 18.6', mode: 'test',
    seed: history.startsWith('b2b-')
      ? 'existing E2E identity seed; real login, inquiry, quote, separate approver, conversion and signed Fake payment HTTP flow'
      : 'existing E2E verified identity seed and real Go password login/session; constrained owned historical orders with all DB guards enabled',
    fault: history.endsWith('-loading') ? 'proxy holds a completed real Go customer orders GET response'
      : history.endsWith('-503') ? 'proxy substitutes HTTP 503 after forwarding real Go customer orders GET'
      : 'none',
    boundary: history === 'history-expired-session' ? 'owned real session expires against DB clock; backend rejects it'
      : history === 'history-not-found' ? 'owned guest-order link removed; real backend ownership predicate returns 404'
      : 'read-only history: disabled action N/A; busy navigation is absent; bigint order number and SKU <=255 characters' };
  const account = /^real-235-(.+)\.png$/.exec(snapshot.split('/').at(-1) ?? '')?.[1];
  if (account) return { application: 'Go', database: 'PostgreSQL 18.6', mode: 'test',
    seed: 'real registration and loopback SMTP notification sink; backend-issued single-use proof',
    fault: account === 'account-in-flight' || account === 'verification-processing'
      ? 'proxy holds a completed real Go POST response'
      : account === 'account-disabled' ? 'proxy substitutes HTTP 503 after forwarding real Go session GET'
      : account === 'account-request-failure' ? 'proxy substitutes HTTP 503 after forwarding real Go session POST'
      : 'none',
    boundary: account === 'verification-expired' ? 'backend-issued proof expired with DB clock in the owned CI database'
      : account === 'verification-replayed' ? 'backend-issued proof already consumed by real verification'
      : 'real backend DTO; fragment cleared before capture; no route interception' };
  const faults: Record<string, string> = {
    'checkout-in-flight': 'proxy holds a completed real Go POST response',
    'checkout-ambiguous-failure': 'proxy discards a committed real Go POST response with HTTP 502',
    'checkout-retry': 'same-key replay after proxy discarded the committed response',
    'completion-loading': 'proxy holds a completed real Go GET response',
    'completion-error': 'proxy substitutes HTTP 503 after forwarding the real Go GET',
  };
  const state = /^real-213-(.+)\.png$/.exec(snapshot.split('/').at(-1) ?? '')?.[1];
  return { application: 'Go', database: 'PostgreSQL 18.6', mode: 'test',
    seed: 'existing E2E SQL and constrained owned order/checkout fixtures',
    fault: faults[state ?? ''] ?? 'none',
    boundary: state === 'completion-long-order' ? 'maximum legal UUID; unbounded order number is not exposed by this page'
      : state === 'completion-pending' ? 'real ready/authorized state; pending_payment/captured is unreachable'
      : 'real backend DTO, no route interception' };
}
