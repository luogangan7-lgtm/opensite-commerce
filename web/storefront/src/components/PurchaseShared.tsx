/** Responsibility: purchase boundary notice and exact service totals.
 * Dependencies: shared UI, purchase contracts/copy and money presentation; callers: purchase islands.
 * Invariants: no locally computed monetary values; fixture notice visible on every purchase surface.
 */
import { Button } from '@opensite/ui';
import type { Preview, PurchaseContext } from '../lib/commerce-contract';
import { commerceCopy } from '../lib/commerce-copy';
import { formatAmount } from '../lib/presentation';
export function PurchaseNotice({ context, locale }: { context: PurchaseContext; locale: string }) {
  const ui = commerceCopy(locale);
  return <>{context.fixture && <p className="purchase-notice" data-fixture-notice="true">{ui.fixtureNotice}</p>}{!context.enabled && <div className="purchase-unavailable">
    <p>{ui.unavailableDescription}</p><Button disabled>{ui.temporarilyUnavailable}</Button>
  </div>}</>;
}
export function PurchaseTotals({ preview, locale }: { preview: Preview; locale: string }) {
  const ui = commerceCopy(locale);
  return <dl className="purchase-totals">
    {(['subtotal', 'tax', 'shipping', 'total'] as const).map(key => <div key={key} className={key === 'total' ? 'purchase-total' : undefined}>
      <dt>{ui[key]}</dt><dd>{formatAmount(preview[`${key}_minor`], preview.currency, locale)}</dd>
    </div>)}
  </dl>;
}
