/** Responsibility: read-only published product detail; dependencies: shared Card.
 * Callers: product SSR route; invariants: the price label is preformatted from sellable SKUs, no inventory counts.
 */
import { Card } from '@opensite/ui';
export default function ProductSummary({ description, price }: { description: string; price: string }) {
  return <Card className="product-summary">
    <p className="product-price">{price}</p>
    <p>{description}</p>
  </Card>;
}
