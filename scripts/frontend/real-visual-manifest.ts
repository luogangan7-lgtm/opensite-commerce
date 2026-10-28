/** Required real-backend visual evidence; consumed by CI and the browser suite.
 * The issue inventory is explicit: deleting a screenshot call cannot silently pass.
 */
export const REAL_VISUAL_STATES = {
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

export function realBackendEvidence(snapshot: string) {
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
