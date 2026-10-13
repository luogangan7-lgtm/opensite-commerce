/** Responsibility: read-only published product detail; dependencies: shared Card/price formatter.
 * Callers: product SSR route; invariants: exact price strings, no cart or inventory claims.
 */
import { Card } from '@opensite/ui';
import { formatAmount } from '../lib/presentation';
export default function ProductSummary({ description, amount, currency, locale }: {
  description: string; amount: string; currency: string; locale: string;
}) {
  return <Card className="product-summary">
    <p className="product-price">{formatAmount(amount, currency, locale)}</p>
    <p>{description}</p>
  </Card>;
}
