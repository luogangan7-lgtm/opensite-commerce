/** Required real-backend visual evidence; consumed by CI and the browser suite.
 * The issue inventory is explicit: deleting a screenshot call cannot silently pass.
 */
export const REAL_VISUAL_STATES = {
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
